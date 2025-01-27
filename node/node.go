package node

import "strings"

// Node - tree structure to record directory contents for a git branch
// This is used so that we can reconcile renames, deletes and copies and for example
// filter out a delete of a renamed file or similar.
// The tree is updated after every commit is processed.
type Node struct {
	Name            string
	Path            string
	IsFile          bool
	CaseInsensitive bool
	Children        []*Node
}

func (n *Node) stringEqual(s1, s2 string) bool {
	if n.CaseInsensitive {
		return len(s1) == len(s2) && strings.EqualFold(s1, s2)
	}
	return len(s1) == len(s2) && s1 == s2
}

func NewNode(name string, caseInsensitive bool) *Node {
	return &Node{Name: name, CaseInsensitive: caseInsensitive}
}

func (n *Node) AddSubFile(fullPath string, subPath string) {
	parts := strings.Split(subPath, "/")
	if len(parts) == 1 {
		for _, c := range n.Children {
			if n.stringEqual(c.Name, parts[0]) {
				return // file already registered
			}
		}
		n.Children = append(n.Children, &Node{Name: parts[0], IsFile: true, Path: fullPath, CaseInsensitive: n.CaseInsensitive})
	} else {
		for _, c := range n.Children {
			if n.stringEqual(c.Name, parts[0]) {
				c.AddSubFile(fullPath, strings.Join(parts[1:], "/"))
				return
			}
		}
		n.Children = append(n.Children, NewNode(parts[0], n.CaseInsensitive))
		n.Children[len(n.Children)-1].AddSubFile(fullPath, strings.Join(parts[1:], "/"))
	}
}

func (n *Node) DeleteSubFile(fullPath string, subPath string) {
	parts := strings.Split(subPath, "/")
	if len(parts) == 1 {
		i := 0
		var c *Node
		found := false
		for i, c = range n.Children {
			if n.stringEqual(c.Name, parts[0]) {
				found = true
				break
			}
		}
		if i < len(n.Children) && found { // Ignore files not found
			n.Children[i] = n.Children[len(n.Children)-1]
			n.Children = n.Children[:len(n.Children)-1]
		}
	} else {
		for _, c := range n.Children {
			if n.stringEqual(c.Name, parts[0]) {
				c.DeleteSubFile(fullPath, strings.Join(parts[1:], "/"))
				return
			}
		}
	}
}

func (n *Node) AddFile(path string) {
	n.AddSubFile(path, path)
}

func (n *Node) DeleteFile(path string) {
	n.DeleteSubFile(path, path)
}

func (n *Node) getChildFiles() []string {
	files := make([]string, 0)
	for _, c := range n.Children {
		if c.IsFile {
			files = append(files, c.Path)
		} else {
			files = append(files, c.getChildFiles()...)
		}
	}
	return files
}

// Return a list of all files in a directory
func (n *Node) GetFiles(dirName string) []string {
	files := make([]string, 0)
	// Root of node tree - just get all files
	if n.Name == "" && dirName == "" {
		files = append(files, n.getChildFiles()...)
		return files
	}
	// Otherwise check directory is one of the children of current node
	parts := strings.Split(dirName, "/")
	if len(parts) == 1 {
		for _, c := range n.Children {
			if n.stringEqual(c.Name, parts[0]) {
				if c.IsFile {
					files = append(files, c.Path)
				} else {
					files = append(files, c.getChildFiles()...)
				}
			}
		}
		return files
	} else {
		for _, c := range n.Children {
			if n.stringEqual(c.Name, parts[0]) {
				return c.GetFiles(strings.Join(parts[1:], "/"))
			}
		}
	}
	return files
}

// Returns true if it finds a single file with specified name
func (n *Node) FindFile(fileName string) bool {
	parts := strings.Split(fileName, "/")
	dir := ""
	if len(parts) > 1 {
		dir = strings.Join(parts[:len(parts)-1], "/")
	}
	files := n.GetFiles(dir)
	for _, f := range files {
		if n.stringEqual(f, fileName) {
			return true
		}
	}
	return false
}
