package convert

import (
	"os"
	"path/filepath"

	"github.com/jenkins-x-plugins/jx-pipeline/pkg/pipelines/processor"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cmdrunner"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/cli"
	"github.com/jenkins-x/jx-helpers/v3/pkg/options"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"
)

// RemoteTasksOptions contains the command line options
type RemoteTasksOptions struct {
	options.BaseOptions

	OverrideSHA                  string
	Dir                          string
	WorkspaceVolumeSize          string
	CalculateWorkspaceVolumeSize bool

	Processor     processor.Interface
	GitClient     gitclient.Interface
	CommandRunner cmdrunner.CommandRunner
}

var (
	remoteTasksCmdLong = templates.LongDesc(`
		Converts the pipelines from the 'image: uses:sourceURI' mechanism to native Tekton.
		
		Existing PipelineRuns are converted into either a new PipelineRun, that uses the Tekton git resolver to
		pull tasks from the sourceURI, or to explicit Tasks based on whether existing PipelineRun has a parent in it's 
		in it's stepTemplate.

		Existing Tasks have the default lighthouse params/envVars (PULL_NUMBER, REPO_NAME etc) appended to them.

		As existing steps are being migrated to tasks a workspace volume needs to be mounted to the tasks. By default the
		size of the workspace is calculated based on the size of the repository + a 300Mi buffer. This can be overridden
		by setting --calculate-workspace-volume=false & --workspace-volume=<size> (if no value is given it defaults to 1Gi)
`)

	remoteTasksCmdExample = templates.Examples(`
		# Convert a repository created using uses: syntax to use the new native Tekton syntax
		jx pipeline convert remotetasks
	`)
)

// NewCmdPipelineConvertRemoteTasks creates the command
func NewCmdPipelineConvertRemoteTasks() (*cobra.Command, *RemoteTasksOptions) {
	o := &RemoteTasksOptions{}

	cmd := &cobra.Command{
		Use:     "remotetasks",
		Short:   "Converts the pipelines to use native Tekton syntax",
		Long:    remoteTasksCmdLong,
		Example: remoteTasksCmdExample,
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	o.BaseOptions.AddBaseFlags(cmd)

	cmd.Flags().StringVarP(&o.OverrideSHA, "sha", "s", "", "Overrides the SHA taken from \"image:uses:\" with the given value")
	cmd.Flags().StringVarP(&o.Dir, "dir", "d", ".", "The directory to look for the pipeline files. Defaults to the current directory")
	cmd.Flags().StringVarP(&o.WorkspaceVolumeSize, "workspace-volume", "v", "", "The size of the workspace volume that backs the pipelines.")
	cmd.Flags().BoolVarP(&o.CalculateWorkspaceVolumeSize, "calculate-workspace-volume", "c", true, "Calculate the workspace volume size based on the size of the repository + a 300Mi buffer. This will override the value set in --workspace-volume")
	return cmd, o
}

// Validate verifies settings
func (o *RemoteTasksOptions) Validate() error {
	err := o.BaseOptions.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate base options")
	}

	if o.CommandRunner == nil {
		o.CommandRunner = cmdrunner.QuietCommandRunner
	}
	if o.GitClient == nil {
		o.GitClient = cli.NewCLIClient("", o.CommandRunner)
	}

	workspaceQuantity, err := o.getWorkspaceQuantity()
	if err != nil {
		return errors.Wrapf(err, "failed to get workspace quantity")
	}

	if o.Processor == nil {
		o.Processor = processor.NewRemoteTasksMigrator(o.OverrideSHA, workspaceQuantity)
	}
	return nil
}

// Run implements this command
func (o *RemoteTasksOptions) Run() error {
	if err := o.Validate(); err != nil {
		return errors.Wrapf(err, "failed to validate options")
	}

	// We need to make sure that the tasks directory is processed first, then the packs directory as the packs directory
	// will reference tasks in the tasks directory
	fs, err := os.ReadDir(o.Dir)
	if err != nil {
		return errors.Wrapf(err, "failed to read dir %s", o.Dir)
	}

	for _, f := range o.sortDirs(fs) {
		if !f.IsDir() {
			continue
		}

		err = filepath.Walk(f.Name(), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info == nil || !info.IsDir() {
				return nil
			}
			return o.ProcessDir(path)
		})
		if err != nil {
			log.Logger().Errorf("failed to process dir %s: %s", f.Name(), err.Error())
		}
	}
	return nil
}

func (o *RemoteTasksOptions) ProcessDir(dir string) error {
	fs, err := os.ReadDir(dir)
	if err != nil {
		return errors.Wrapf(err, "failed to read dir %s", dir)
	}

	for _, f := range fs {
		if filepath.Ext(f.Name()) != ".yaml" {
			continue
		}

		path := filepath.Join(dir, f.Name())
		_, err = processor.ProcessFile(o.Processor, path)
		if err != nil {
			log.Logger().Errorf("failed to process file %s: %s", path, err.Error())
		}
	}
	return nil
}

// sortDirs sorts the directories so that the tasks and packs directories are processed first
func (o *RemoteTasksOptions) sortDirs(dirs []os.DirEntry) []os.DirEntry {
	dirs = o.moveDirEntryInSliceToIndex(dirs, "tasks", 0)
	dirs = o.moveDirEntryInSliceToIndex(dirs, "packs", 1)
	return dirs
}
func (o *RemoteTasksOptions) moveDirEntryInSliceToIndex(slice []os.DirEntry, name string, index int) []os.DirEntry {
	for i, dir := range slice {
		if dir.Name() == name {
			slice = append(slice[:i], slice[i+1:]...)
			slice = append(slice[:index], append([]os.DirEntry{dir}, slice[index:]...)...)
			break
		}
	}
	return slice
}

func (o *RemoteTasksOptions) getWorkspaceQuantity() (resource.Quantity, error) {
	if o.WorkspaceVolumeSize != "" {
		volumeSize, err := resource.ParseQuantity(o.WorkspaceVolumeSize)
		if err != nil {
			return resource.Quantity{}, errors.Wrapf(err, "failed to parse workspace volume size %s", o.WorkspaceVolumeSize)
		}
		return volumeSize, nil
	}
	if o.CalculateWorkspaceVolumeSize {
		volumeSize, err := o.calculateWorkspaceVolumeFromRepo()
		if err != nil {
			return resource.Quantity{}, errors.Wrapf(err, "failed to calculate workspace volume size")
		}
		return volumeSize, nil
	}
	return resource.MustParse("1Gi"), nil
}

func (o *RemoteTasksOptions) calculateWorkspaceVolumeFromRepo() (resource.Quantity, error) {
	packSize, err := gitclient.GetSizePack(o.GitClient, o.Dir)
	if err != nil {
		return resource.Quantity{}, err
	}
	// Add a 300Mi buffer to the pack size to account for any additional files that may be added during the pipeline
	packSize += 300 << 20
	q := resource.NewQuantity(packSize, resource.BinarySI)
	return *q, err
}
