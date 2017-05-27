// Copyright 2015 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package git

import (
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// EntryMode the type of the object in the git tree
type EntryMode int

// There are only a few file modes in Git. They look like unix file modes, but they can only be
// one of these.
const (
	// EntryModeBlob
	EntryModeBlob EntryMode = 0100644
	// EntryModeExec
	EntryModeExec EntryMode = 0100755
	// EntryModeSymlink
	EntryModeSymlink EntryMode = 0120000
	// EntryModeCommit
	EntryModeCommit EntryMode = 0160000
	// EntryModeTree
	EntryModeTree EntryMode = 0040000
)

// TreeEntry the leaf in the git tree
type TreeEntry struct {
	ID   SHA1
	Type ObjectType

	mode EntryMode
	name string

	ptree *Tree

	commited bool

	size  int64
	sized bool
}

// Name returns the name of the entry
func (te *TreeEntry) Name() string {
	return te.name
}

// Size returns the size of the entry
func (te *TreeEntry) Size() int64 {
	if te.IsDir() {
		return 0
	} else if te.sized {
		return te.size
	}

	stdout, err := NewCommand("cat-file", "-s", te.ID.String()).RunInDir(te.ptree.repo.Path)
	if err != nil {
		return 0
	}

	te.sized = true
	te.size, _ = strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	return te.size
}

// IsSubModule if the entry is a sub module
func (te *TreeEntry) IsSubModule() bool {
	return te.mode == EntryModeCommit
}

// IsDir if the entry is a sub dir
func (te *TreeEntry) IsDir() bool {
	return te.mode == EntryModeTree
}

// IsLink if the entry is a symlink
func (te *TreeEntry) IsLink() bool {
	return te.mode == EntryModeSymlink
}

// Blob retrun the blob object the entry
func (te *TreeEntry) Blob() *Blob {
	return &Blob{
		repo:      te.ptree.repo,
		TreeEntry: te,
	}
}

// GetSubJumpablePathName return the full path of subdirectory jumpable ( contains only one directory )
func (te *TreeEntry) GetSubJumpablePathName() string {
	if te.IsSubModule() || !te.IsDir() {
		return ""
	}
	tree, err := te.ptree.SubTree(te.name)
	if err != nil {
		return te.name
	}
	entries, _ := tree.ListEntries()
	if len(entries) == 1 && entries[0].IsDir() {
		name := entries[0].GetSubJumpablePathName()
		if name != "" {
			return te.name + "/" + name
		}
	}
	return te.name
}

// Entries a list of entry
type Entries []*TreeEntry

var sorter = []func(t1, t2 *TreeEntry) bool{
	func(t1, t2 *TreeEntry) bool {
		return (t1.IsDir() || t1.IsSubModule()) && !t2.IsDir() && !t2.IsSubModule()
	},
	func(t1, t2 *TreeEntry) bool {
		return t1.name < t2.name
	},
}

func (tes Entries) Len() int      { return len(tes) }
func (tes Entries) Swap(i, j int) { tes[i], tes[j] = tes[j], tes[i] }
func (tes Entries) Less(i, j int) bool {
	t1, t2 := tes[i], tes[j]
	var k int
	for k = 0; k < len(sorter)-1; k++ {
		s := sorter[k]
		switch {
		case s(t1, t2):
			return true
		case s(t2, t1):
			return false
		}
	}
	return sorter[k](t1, t2)
}

// Sort sort the list of entry
func (tes Entries) Sort() {
	sort.Sort(tes)
}

type taskResult struct {
	commit string
	paths  []string
}

func logCommand(currentCommit, treePath string) *Command {
	var commitHash string
	if len(currentCommit) == 0 {
		commitHash = "HEAD"
	} else {
		commitHash = currentCommit + "^"
	}
	return NewCommand("log", prettyLogFormat, "--name-only", "-2", commitHash, "--", treePath)
}

func getCommitInfos(headCommit *Commit, currentCommit, treePath string) (*taskResult, error) {
	logOutput, err := logCommand(currentCommit, treePath).RunInDir(headCommit.repo.Path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(logOutput, "\n")
	paths := make([]string, len(lines)) //TODO
	/*
		i := 0
		for i < len(lines) {
			state.nextCommit(lines[i])
			i++
			for ; i < len(lines); i++ {
				path := lines[i]
				if path == "" {
					break
				}
				state.update(path)
			}
			i++ // skip blank line
			if len(state.entries) == len(state.commits) {
				break
			}
		}
	*/
	return &taskResult{
		commit: currentCommit,
		paths:  paths,
	}, nil
}

// GetCommitsInfoWithCustomConcurrency takes advantages of concurrency to speed up getting information
func (tes Entries) GetCommitsInfoWithCustomConcurrency(headCommit *Commit, treePath string, maxConcurrency int) ([][]interface{}, error) {
	//Init
	commitsInfo := make([][]interface{}, len(tes))             //TODO
	commitsMapInfo := make(map[string][]interface{}, len(tes)) //TODO
	if maxConcurrency <= 0 {
		maxConcurrency = runtime.NumCPU()
	}
	done := make(chan bool)
	chanTask := make(chan taskResult, maxConcurrency)
	chanResponse := make(chan taskResult, maxConcurrency*10) //TODO find perferct size
	nbStarted := 0
	nbRunning := 0
	nbCommitParsing := 4
	nextPathMissing := 0 //Index in tes entry of next not found entry

	//Start thread for parsing
	go func() {
		for result := range chanResponse { //TODO check nil
			for _, path := range result.paths {
				relPath, err := filepath.Rel(treePath, path)
				log("%v %v", relPath, err)
			}
			if len(tes) == len(commitsInfo) {
				break //Finish line
			}
		}
		done <- true
	}()

	//Start threads if we miss information
	for len(tes) > len(commitsInfo) {
		if (len(tes) - len(commitsInfo)) <= (maxConcurrency + nbStarted) { //We have only few file to found commit compared to allready run and number of goroutine //TODO analyze
			go func() {
				for ; nextPathMissing < len(tes); nextPathMissing++ {
					if _, ok := commitsMapInfo[tes[nextPathMissing].Name()]; !ok {
						break //Found the nextPathMissing
					}
				}
				//TODO detect end and multiple access to nextPathMissing
				c, err := headCommit.GetCommitByPath(filepath.Join(treePath, tes[nextPathMissing].Name()))
				chanTask <- err
				chanResponse <- taskResult{
					commit: c,
					paths:  []string{tes[i].Name()},
				}
			}()
		} else {
			go func() {
				currentCommit := headCommit.ID.String() //TODO maybe used HEAD~4 ????
				r, err := getCommitInfos(headCommit, currentCommit, treePath)
				chanTask <- err
				chanResponse <- r
			}()
		}
		nbRunning++
		nbStarted++

		if nbRunning >= maxConcurrency || (len(tes)-len(commitsInfo)) <= (nbStarted) { //Wait for a routine to finish because max running or waiting for end //TODO analyze
			err <- chanTask
			if err != nil {
				return nil, err
			}
			nbRunning--
		}
		if nbStarted%maxConcurrency == 0 && nbStarted > 0 {
			nbCommitParsing *= 2
		}
	}

	//TODO handle submodule
	//TODO check that all go routine are finished
	<-done
	return commitsInfo, nil
}

// GetCommitsInfo gets information of all commits that are corresponding to these entries
func (tes Entries) GetCommitsInfo(commit *Commit, treePath string) ([][]interface{}, error) {
	return tes.GetCommitsInfoWithCustomConcurrency(commit, treePath, 0)
}
