package commands

import (
	"fmt"
	"os"

	"github.com/go-errors/errors"
	"github.com/jesseduffield/lazygit/pkg/commands/models"
	"github.com/jesseduffield/lazygit/pkg/commands/oscommands"
	. "github.com/jesseduffield/lazygit/pkg/commands/types"
	"github.com/jesseduffield/lazygit/pkg/gui/filetree"
)

//counterfeiter:generate . IWorktreeMgr
type IWorktreeMgr interface {
	LoadStatusFiles(opts LoadStatusFilesOpts) []*models.File
	OpenMergeToolCmdObj() ICmdObj
	StageFile(fileName string) error
	StageAll() error
	UnstageAll() error
	UnStageFile(fileNames []string, reset bool) error
	DiscardAllFileChanges(file *models.File) error
	DiscardAllDirChanges(node *filetree.FileNode) error
	DiscardUnstagedDirChanges(node *filetree.FileNode) error
	DiscardUnstagedFileChanges(file *models.File) error
	Ignore(filename string) error
	CheckoutFile(commitSha, fileName string) error
	DiscardAnyUnstagedFileChanges() error
	RemoveTrackedFiles(name string) error
	RemoveUntrackedFiles() error
	ResetAndClean() error
	EditFileCmdObj(filename string) (ICmdObj, error)
}

type WorktreeMgr struct {
	*MgrCtx

	statusFilesLoader *StatusFilesLoader
	branchesMgr       IBranchesMgr
	submodulesMgr     ISubmodulesMgr
}

func NewWorktreeMgr(mgrCtx *MgrCtx, branchesMgr IBranchesMgr, submodulesMgr ISubmodulesMgr) *WorktreeMgr {
	return &WorktreeMgr{
		MgrCtx:            mgrCtx,
		statusFilesLoader: NewStatusFilesLoader(mgrCtx),
		branchesMgr:       branchesMgr,
		submodulesMgr:     submodulesMgr,
	}
}

func (c *WorktreeMgr) LoadStatusFiles(opts LoadStatusFilesOpts) []*models.File {
	return c.statusFilesLoader.Load(opts)
}

func (c *WorktreeMgr) OpenMergeToolCmdObj() ICmdObj {
	return BuildGitCmdObjFromStr("mergetool")
}

// StageFile stages a file
func (c *WorktreeMgr) StageFile(fileName string) error {
	return c.RunGitCmdFromStr(fmt.Sprintf("add -- %s", c.Quote(fileName)))
}

// StageAll stages all files
func (c *WorktreeMgr) StageAll() error {
	return c.RunGitCmdFromStr("add -A")
}

// UnstageAll unstages all files
func (c *WorktreeMgr) UnstageAll() error {
	return c.RunGitCmdFromStr("reset")
}

// UnStageFile unstages a file
// we accept an array of filenames for the cases where a file has been renamed i.e.
// we accept the current name and the previous name
func (c *WorktreeMgr) UnStageFile(fileNames []string, reset bool) error {
	cmdFormat := "rm --cached --force -- %s"
	if reset {
		cmdFormat = "reset HEAD -- %s"
	}

	for _, name := range fileNames {
		if err := c.RunGitCmdFromStr(fmt.Sprintf(cmdFormat, c.Quote(name))); err != nil {
			return err
		}
	}
	return nil
}

func (c *WorktreeMgr) beforeAndAfterFileForRename(file *models.File) (*models.File, *models.File, error) {
	if !file.IsRename() {
		return nil, nil, errors.New("Expected renamed file")
	}

	// we've got a file that represents a rename from one file to another. Here we will refetch
	// all files, passing the --no-renames flag and then recursively call the function
	// again for the before file and after file.

	filesWithoutRenames := c.LoadStatusFiles(LoadStatusFilesOpts{NoRenames: true})
	var beforeFile *models.File
	var afterFile *models.File
	for _, f := range filesWithoutRenames {
		if f.Name == file.PreviousName {
			beforeFile = f
		}

		if f.Name == file.Name {
			afterFile = f
		}
	}

	if beforeFile == nil || afterFile == nil {
		return nil, nil, errors.New("Could not find deleted file or new file for file rename")
	}

	if beforeFile.IsRename() || afterFile.IsRename() {
		// probably won't happen but we want to ensure we don't get an infinite loop
		return nil, nil, errors.New("Nested rename found")
	}

	return beforeFile, afterFile, nil
}

// DiscardAllFileChanges directly
func (c *WorktreeMgr) DiscardAllFileChanges(file *models.File) error {
	if file.IsRename() {
		beforeFile, afterFile, err := c.beforeAndAfterFileForRename(file)
		if err != nil {
			return err
		}

		if err := c.DiscardAllFileChanges(beforeFile); err != nil {
			return err
		}

		if err := c.DiscardAllFileChanges(afterFile); err != nil {
			return err
		}

		return nil
	}

	quotedFileName := c.Quote(file.Name)

	if file.ShortStatus == "AA" {
		if err := c.RunGitCmdFromStr(fmt.Sprintf("checkout --ours --  %s", quotedFileName)); err != nil {
			return err
		}
		if err := c.RunGitCmdFromStr(fmt.Sprintf("add %s", quotedFileName)); err != nil {
			return err
		}
		return nil
	}

	if file.ShortStatus == "DU" {
		return c.RunGitCmdFromStr(fmt.Sprintf("rm %s", quotedFileName))
	}

	// if the file isn't tracked, we assume you want to delete it
	if file.HasStagedChanges || file.HasMergeConflicts {
		if err := c.RunGitCmdFromStr(fmt.Sprintf("reset -- %s", quotedFileName)); err != nil {
			return err
		}
	}

	if file.ShortStatus == "DD" || file.ShortStatus == "AU" {
		return nil
	}

	if file.Added {
		return c.os.RemoveFile(file.Name)
	}
	return c.DiscardUnstagedFileChanges(file)
}

func (c *WorktreeMgr) DiscardAllDirChanges(node *filetree.FileNode) error {
	// this could be more efficient but we would need to handle all the edge cases
	return node.ForEachFile(c.DiscardAllFileChanges)
}

func (c *WorktreeMgr) DiscardUnstagedDirChanges(node *filetree.FileNode) error {
	if err := c.removeUntrackedDirFiles(node); err != nil {
		return err
	}

	quotedPath := c.Quote(node.GetPath())
	if err := c.RunGitCmdFromStr(fmt.Sprintf("checkout -- %s", quotedPath)); err != nil {
		return err
	}

	return nil
}

func (c *WorktreeMgr) removeUntrackedDirFiles(node *filetree.FileNode) error {
	untrackedFilePaths := node.GetPathsMatching(
		func(n *filetree.FileNode) bool { return n.File != nil && !n.File.GetIsTracked() },
	)

	for _, path := range untrackedFilePaths {
		err := os.Remove(path)
		if err != nil {
			return err
		}
	}

	return nil
}

// DiscardUnstagedFileChanges directly
func (c *WorktreeMgr) DiscardUnstagedFileChanges(file *models.File) error {
	quotedFileName := c.Quote(file.Name)
	return c.RunGitCmdFromStr(fmt.Sprintf("checkout -- %s", quotedFileName))
}

// Ignore adds a file to the gitignore for the repo
func (c *WorktreeMgr) Ignore(filename string) error {
	return c.os.AppendLineToFile(".gitignore", filename)
}

// CheckoutFile checks out the file for the given commit
func (c *WorktreeMgr) CheckoutFile(commitSha, fileName string) error {
	return c.RunGitCmdFromStr(fmt.Sprintf("checkout %s %s", commitSha, fileName))
}

// DiscardAnyUnstagedFileChanges discards any unstages file changes via `git checkout -- .`
func (c *WorktreeMgr) DiscardAnyUnstagedFileChanges() error {
	return c.RunGitCmdFromStr("checkout -- .")
}

// RemoveTrackedFiles will delete the given file(s) even if they are currently tracked
func (c *WorktreeMgr) RemoveTrackedFiles(name string) error {
	return c.RunGitCmdFromStr(fmt.Sprintf("rm -r --cached %s", name))
}

// RemoveUntrackedFiles runs `git clean -fd`
func (c *WorktreeMgr) RemoveUntrackedFiles() error {
	return c.RunGitCmdFromStr("clean -fd")
}

// ResetAndClean removes all unstaged changes and removes all untracked files
func (c *WorktreeMgr) ResetAndClean() error {
	submoduleConfigs, err := c.submodulesMgr.GetConfigs()
	if err != nil {
		return err
	}

	if len(submoduleConfigs) > 0 {
		if err := c.submodulesMgr.StashAndReset(submoduleConfigs); err != nil {
			return err
		}
	}

	if err := c.branchesMgr.ResetToRef("HEAD", HARD, ResetToRefOpts{}); err != nil {
		return err
	}

	return c.RemoveUntrackedFiles()
}

func (c *WorktreeMgr) EditFileCmdObj(filename string) (ICmdObj, error) {
	editor := c.config.GetUserConfig().OS.EditCommand

	if editor == "" {
		editor = c.config.GetConfigValue("core.editor")
	}

	if editor == "" {
		editor = c.os.Getenv("GIT_EDITOR")
	}
	if editor == "" {
		editor = c.os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = c.os.Getenv("EDITOR")
	}
	if editor == "" {
		if err := c.Run(oscommands.NewCmdObjFromStr("which vi")); err == nil {
			editor = "vi"
		}
	}
	if editor == "" {
		return nil, errors.New("No editor defined in config file, $GIT_EDITOR, $VISUAL, $EDITOR, or git config")
	}

	cmdObj := c.BuildShellCmdObj(fmt.Sprintf("%s %s", editor, c.Quote(filename)))

	return cmdObj, nil
}