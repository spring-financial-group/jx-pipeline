package processor

import (
	"context"
	"github.com/jenkins-x/jx-helpers/v3/pkg/yamls"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/jenkins-x/lighthouse-client/pkg/triggerconfig/inrepo"
	"github.com/pkg/errors"
	tekv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"path/filepath"
	"reflect"
	"sigs.k8s.io/yaml"
)

var (
	LighthouseTaskParams = []v1beta1.ParamSpec{
		{
			Name:        "BUILD_ID",
			Type:        "string",
			Description: "The unique build number",
			Default:     nil,
		},
		{
			Name:        "JOB_NAME",
			Type:        "string",
			Description: "The fileName of the job which is the trigger context fileName",
			Default:     nil,
		},
		{
			Name:        "JOB_SPEC",
			Type:        "string",
			Description: "The specification of the job",
			Default:     nil,
		},
		{
			Name:        "JOB_TYPE",
			Type:        "string",
			Description: "The kind of the job: postsubmit or presubmit",
			Default:     nil,
		},
		{
			Name:        "PULL_BASE_REF",
			Type:        "string",
			Description: "The base git reference of the pull request",
			Default:     nil,
		},
		{
			Name:        "PULL_BASE_SHA",
			Type:        "string",
			Description: "The git sha of the base of the pull request",
			Default:     nil,
		},
		{
			Name:        "PULL_NUMBER",
			Type:        "string",
			Description: "The git pull request number",
			Default:     &v1beta1.ParamValue{Type: "string", StringVal: ""},
		},
		{
			Name:        "PULL_PULL_REF",
			Type:        "string",
			Description: "The git pull request ref in the form 'refs/pull/$PULL_NUMBER/head'",
			Default:     &v1beta1.ParamValue{Type: "string", StringVal: ""},
		},
		{
			Name:        "PULL_PULL_SHA",
			Type:        "string",
			Description: "The git pull reference strings of base and latest in the form 'master:$PULL_BASE_SHA,$PULL_NUMBER:$PULL_PULL_SHA:refs/pull/$PULL_NUMBER/head'",
			Default:     nil,
		},
		{
			Name:        "PULL_REFS",
			Type:        "string",
			Description: "The git pull reference strings of base and latest in the form 'master:$PULL_BASE_SHA,$PULL_NUMBER:$PULL_PULL_SHA:refs/pull/$PULL_NUMBER/head'",
			Default:     nil,
		},
		{
			Name:        "REPO_NAME",
			Type:        "string",
			Description: "The git repository fileName",
			Default:     nil,
		},
		{
			Name:        "REPO_OWNER",
			Type:        "string",
			Description: "The git repository owner (user or organisation)",
			Default:     nil,
		},
		{
			Name:        "REPO_URL",
			Type:        "string",
			Description: "The URL of the git repo to clone",
			Default:     nil,
		},
	}

	LighthouseEnvs = ParamsToEnvVars(LighthouseTaskParams)

	HomeEnv = v1.EnvVar{
		Name:  "HOME",
		Value: "/workspace",
	}

	trueRef = true

	DefaultEnvFroms = []v1.EnvFromSource{
		{
			SecretRef: &v1.SecretEnvSource{
				LocalObjectReference: v1.LocalObjectReference{
					Name: "jx-boot-job-env-vars",
				},
				Optional: &trueRef,
			},
		},
	}

	serviceAccountName = "tekton-bot"
)

type RemoteTasksMigrator struct {
	workspaceVolumeQuantity resource.Quantity
	gitResolver             *GitRefResolver
}

// NewRemoteTasksMigrator creates a new uses migrator
func NewRemoteTasksMigrator(workspaceVolumeQuantity resource.Quantity, gitResolver *GitRefResolver) *RemoteTasksMigrator {
	return &RemoteTasksMigrator{workspaceVolumeQuantity: workspaceVolumeQuantity, gitResolver: gitResolver}
}

func (p *RemoteTasksMigrator) ProcessPipeline(pipeline *v1beta1.Pipeline, path string) (bool, error) {
	return false, nil
}

// ProcessPipelineRun processes a PipelineRun and migrates it to either a new PipelineRun or to individual Tasks
// based on whether it is a parent or child PipelineRun
func (p *RemoteTasksMigrator) ProcessPipelineRun(prs *v1beta1.PipelineRun, path string) (bool, error) {
	log.Logger().Infof("Processing pipeline run %s", path)
	if taskCount := len(prs.Spec.PipelineSpec.Tasks); taskCount != 1 {
		// All jx pipelines should only have one task
		return false, errors.Errorf("pipeline run %s has %d tasks. Expecting 1", path, taskCount)
	}

	stepTemplate := prs.Spec.PipelineSpec.Tasks[0].TaskSpec.StepTemplate
	if stepTemplate == nil {
		// If there is no step template then we can assume that the pipeline run is a parent pipeline run and
		// so we should migrate it to individual tasks
		log.Logger().Infof("No step template found. Migrating it to individual tasks.")
		return p.migrateToTasks(prs, path)
	}

	prsParent, err := p.gitResolver.NewRefFromUsesImage(stepTemplate.Image, "")
	if err != nil {
		return false, errors.Wrapf(err, "failed to create new ref from uses image %s", stepTemplate.Image)
	}
	if prsParent != nil {
		// If the step template image is a uses image then we can assume that the pipeline run is a child pipeline run,
		// and so we should migrate it to a new pipeline run
		log.Logger().Infof("StepTemplate has image \"%s\". Migrating to new pipelineRun", stepTemplate.Image)
		return p.migrateToNewPipelineRun(prs)
	}

	// Otherwise we can assume that the pipeline run is a parent pipeline run, and so we should migrate it to individual tasks
	log.Logger().Infof("StepTemplate has image \"%s\". Migrating to individual tasks", stepTemplate.Image)
	return p.migrateToTasks(prs, path)
}

// migrateToNewPipelineRun takes a PipelineRun and migrates it to a native Tekton PipelineRun converting the steps
// of the original to individual Tasks within the new PipelineRun. This overwrites the original PipelineRun found at path.
func (p *RemoteTasksMigrator) migrateToNewPipelineRun(prs *v1beta1.PipelineRun) (bool, error) {
	steps := prs.Spec.PipelineSpec.Tasks[0].TaskSpec.Steps
	newPrs := v1beta1.PipelineRun{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "tekton.dev/v1beta1",
			Kind:       "PipelineRun",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: prs.Name,
		},
		Spec: v1beta1.PipelineRunSpec{
			PipelineSpec: &v1beta1.PipelineSpec{
				Workspaces: []v1beta1.PipelineWorkspaceDeclaration{
					{
						Name: "pipeline-ws",
					},
				},
			},
			ServiceAccountName: serviceAccountName,
			Timeout:            prs.Spec.Timeout,
			Workspaces: []v1beta1.WorkspaceBinding{
				{
					Name: "pipeline-ws",
					VolumeClaimTemplate: &v1.PersistentVolumeClaim{
						Spec: v1.PersistentVolumeClaimSpec{
							AccessModes: []v1.PersistentVolumeAccessMode{
								v1.ReadWriteOnce,
							},
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceStorage: p.workspaceVolumeQuantity,
								},
							},
						},
					},
				},
			},
		},
	}

	pipelineTasks := make([]v1beta1.PipelineTask, len(steps))
	for idx := range steps {
		newPipelineTask, err := p.NewPipelineTaskFromStepAndPipelineRun(&steps[idx], prs)
		if err != nil {
			return false, err
		}
		if idx > 0 {
			// If this is not the first step then we need to set the run after to the fileName of the previous task
			newPipelineTask.RunAfter = []string{pipelineTasks[idx-1].Name}
		}
		pipelineTasks[idx] = newPipelineTask
	}
	newPrs.Spec.PipelineSpec.Tasks = pipelineTasks

	if err := newPrs.DeepCopy().Validate(context.Background()); err != nil {
		return false, err
	}
	*prs = newPrs
	return true, nil
}

// migrateToTasks takes a PipelineRun and migrates the steps of the original to individual Tasks. This writes the
// new Tasks to a subdirectory in path named the same as the original PipelineRun. The original PipelineRun is not
// modified.
func (p *RemoteTasksMigrator) migrateToTasks(prs *v1beta1.PipelineRun, path string) (bool, error) {
	steps := prs.Spec.PipelineSpec.Tasks[0].TaskSpec.Steps

	subDir := filepath.Join(filepath.Dir(path), prs.Name)
	err := os.MkdirAll(subDir, 0700)
	if err != nil {
		return false, err
	}

	tasks := make([]v1beta1.Task, len(steps))
	for idx := range steps {
		newTask, err := p.NewTaskFromStepAndPipelineRun(&steps[idx], prs, false)
		if err != nil {
			return false, errors.Wrapf(err, "failed to create task from step[%d] \"%s\"", idx, steps[idx].Name)
		}

		if err := yamls.SaveFile(newTask, filepath.Join(subDir, newTask.Name+".yaml")); err != nil {
			return false, err
		}
		tasks[idx] = newTask
	}
	return false, nil
}

func (p *RemoteTasksMigrator) populateTaskValuesFromPipelineRun(task *v1beta1.Task, pr *v1beta1.PipelineRun, isEmbeddedTask bool) {
	if task.Spec.StepTemplate == nil {
		task.Spec.StepTemplate = &v1beta1.StepTemplate{}
	}

	task.Spec.StepTemplate.WorkingDir = pr.Spec.PipelineSpec.Tasks[0].TaskSpec.TaskSpec.StepTemplate.WorkingDir

	if !isEmbeddedTask {
		// Embedded tasks don't need to have the default params added as they get populated by lighthouse
		p.appendDefaultValues(task)
	}
}

func (p *RemoteTasksMigrator) appendDefaultValues(task *v1beta1.Task) {
	if task.Spec.StepTemplate == nil {
		task.Spec.StepTemplate = &v1beta1.StepTemplate{}
	}

	task.Spec.Params = AppendParamsIfNotPresent(task.Spec.Params, LighthouseTaskParams)
	task.Spec.StepTemplate.EnvFrom = AppendEnvsFromIfNotPresent(task.Spec.StepTemplate.EnvFrom, DefaultEnvFroms)

	task.Spec.StepTemplate.Env = AppendEnvsIfNotPresent(task.Spec.StepTemplate.Env, LighthouseEnvs)
	// We need to replace the HOME env if it's already present in the task as we're moving it from
	// /tekton/home to /workspace
	task.Spec.StepTemplate.Env = ReplaceOrAppendEnv(task.Spec.StepTemplate.Env, HomeEnv)
}

func (p *RemoteTasksMigrator) NewPipelineTaskFromStepAndPipelineRun(step *v1beta1.Step, prs *v1beta1.PipelineRun) (v1beta1.PipelineTask, error) {
	stepParentRef, err := p.gitResolver.NewRefFromUsesImage(step.Image, step.Name)
	if err != nil {
		return v1beta1.PipelineTask{}, err
	}

	if stepParentRef != nil {
		// If the step has a parent then we can just convert it to a pipeline task
		return p.pipelineTaskFromParentRef(step, stepParentRef.GetParentFileName(), stepParentRef)
	}

	if step.Image != "" {
		// If the step has no parent and also has an image then it's a root step and can just be converted to an embedded task
		return p.pipelineTaskFromStep(step, prs)
	}

	// Otherwise it's a child step and is inherited from the parent pipelineRun in the stepTemplate
	stepTemplateParentRef, err := p.gitResolver.NewRefFromUsesImage(prs.Spec.PipelineSpec.Tasks[0].TaskSpec.StepTemplate.Image, step.Name)
	if err != nil {
		return v1beta1.PipelineTask{}, err
	}

	return p.pipelineTaskFromParentRef(step, step.Name, stepTemplateParentRef)
}

func (p *RemoteTasksMigrator) pipelineTaskFromStep(step *v1beta1.Step, prs *v1beta1.PipelineRun) (v1beta1.PipelineTask, error) {
	task, err := p.NewTaskFromStepAndPipelineRun(step, prs, true)
	if err != nil {
		return v1beta1.PipelineTask{}, err
	}
	return v1beta1.PipelineTask{
		Name: step.Name,
		TaskSpec: &v1beta1.EmbeddedTask{
			TaskSpec: task.Spec,
		},
	}, nil
}

func (p *RemoteTasksMigrator) pipelineTaskFromParentRef(step *v1beta1.Step, name string, ref *GitRef) (v1beta1.PipelineTask, error) {
	if step.Name == "" || !p.isStepOverriding(step) {
		// If the child step isn't overriding the parent then we can create a resolver ref
		return v1beta1.PipelineTask{
			Name: name,
			TaskRef: &v1beta1.TaskRef{
				ResolverRef: ref.ToResolverRef(),
			},
			Workspaces: []v1beta1.WorkspacePipelineTaskBinding{
				{Name: "output", Workspace: "pipeline-ws"},
			},
		}, nil
	}

	// Otherwise, we need create an embedded task that applies the original step modifications to the resolved task.
	task, err := p.embeddedTaskFromGitRefAndOverrideStep(step, ref)
	if err != nil {
		return v1beta1.PipelineTask{}, err
	}

	return v1beta1.PipelineTask{
		Name:     name,
		TaskSpec: task,
	}, nil
}

func (p *RemoteTasksMigrator) isStepOverriding(step *v1beta1.Step) bool {
	inStep := &v1beta1.Step{}
	ogStep := inStep.DeepCopy()
	inrepo.OverrideStep(inStep, step)
	return !reflect.DeepEqual(inStep, ogStep)
}

// NewTaskFromStepAndPipelineRun takes a step and a PipelineRun and creates a Task from them.
func (p *RemoteTasksMigrator) NewTaskFromStepAndPipelineRun(step *v1beta1.Step, prs *v1beta1.PipelineRun, isEmbeddedTask bool) (v1beta1.Task, error) {
	taskStep := step

	workingDir := step.WorkingDir
	if workingDir == "" {
		// if the step does not have a working dir then we need to use the working dir from the parent pipeline run
		workingDir = prs.Spec.PipelineSpec.Tasks[0].TaskSpec.TaskSpec.StepTemplate.WorkingDir
	}

	newTask := v1beta1.Task{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Task",
			APIVersion: "tekton.dev/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: step.Name,
		},
		Spec: v1beta1.TaskSpec{
			Steps: []v1beta1.Step{
				*taskStep,
			},
			StepTemplate: &v1beta1.StepTemplate{
				WorkingDir: workingDir,
			},
			Workspaces: []v1beta1.WorkspaceDeclaration{
				{
					Name:        "output",
					MountPath:   "/workspace",
					Description: "The workspace used to store the cloned git repository and the generated files",
				},
			},
		},
	}

	p.populateTaskValuesFromPipelineRun(&newTask, prs, isEmbeddedTask)
	if err := newTask.DeepCopy().Validate(context.Background()); err != nil {
		return v1beta1.Task{}, errors.Wrap(err, "failed to validate new task")
	}
	return newTask, nil
}

// ProcessTask processes a task and appends the default params and environment variables to it
func (p *RemoteTasksMigrator) ProcessTask(task *v1beta1.Task, path string) (bool, error) {
	log.Logger().Infof("Processing task %s", path)
	// We need to add all the default params to the task
	p.appendDefaultValues(task)
	err := task.DeepCopy().Validate(context.Background())
	if err != nil {
		return false, errors.Wrap(err, "failed to validate task")
	}
	return true, nil
}

func (p *RemoteTasksMigrator) ProcessTaskRun(tr *v1beta1.TaskRun, path string) (bool, error) {
	return false, nil
}

func (p *RemoteTasksMigrator) embeddedTaskFromGitRefAndOverrideStep(overrideStep *v1beta1.Step, gitRef *GitRef) (*v1beta1.EmbeddedTask, error) {
	embedded := &v1beta1.EmbeddedTask{}
	switch GetKindFromData(gitRef.ResolvedFile) {
	case "Task":
		task := &tekv1.Task{}
		err := yaml.Unmarshal(gitRef.ResolvedFile, task)
		if err != nil {
			return nil, err
		}

		err = embedded.ConvertFrom(context.Background(), &task.Spec)
		if err != nil {
			return nil, err
		}

		OverrideEmbeddedTaskWithStep(embedded, overrideStep)

	case "PipelineRun":
		pipelineRun := &v1beta1.PipelineRun{}
		err := yaml.Unmarshal(gitRef.ResolvedFile, pipelineRun)
		if err != nil {
			return nil, err
		}

		steps := pipelineRun.Spec.PipelineSpec.Tasks[0].TaskSpec.Steps
		var step *v1beta1.Step
		for _, st := range steps {
			if st.Name == gitRef.StepName {
				step = &st
				break
			}
		}

		inrepo.OverrideStep(step, overrideStep)

		task, err := p.NewTaskFromStepAndPipelineRun(step, pipelineRun, true)
		if err != nil {
			return nil, err
		}

		embedded = &v1beta1.EmbeddedTask{
			TaskSpec: task.Spec,
		}
	default:
		return nil, errors.Errorf("unsupported kind %s", GetKindFromData(gitRef.ResolvedFile))
	}

	// We can remove the default lighthouse params etc. as the task is embedded so these will be populated by
	// lighthouse on the fly
	embedded.TaskSpec.StepTemplate.Env = RemoveEnvs(embedded.TaskSpec.StepTemplate.Env, LighthouseEnvs)
	embedded.TaskSpec.StepTemplate.Env = RemoveEnvs(embedded.TaskSpec.StepTemplate.Env, []v1.EnvVar{HomeEnv})
	embedded.TaskSpec.Params = RemoveParams(embedded.TaskSpec.Params, LighthouseTaskParams)
	embedded.TaskSpec.StepTemplate.EnvFrom = RemoveEnvsFrom(embedded.TaskSpec.StepTemplate.EnvFrom, DefaultEnvFroms)

	return embedded, nil
}

func OverrideEmbeddedTaskWithStep(embeddedTask *v1beta1.EmbeddedTask, overrideStep *v1beta1.Step) {
	if len(embeddedTask.Steps) != 1 {
		log.Logger().Warnf("cannot override task with step. Expected 1 step in task, found %d", len(embeddedTask.Steps))
		return
	}
	inrepo.OverrideStep(&embeddedTask.Steps[0], overrideStep)
}
