package module

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/terraform/config"
)

// Tree represents the module import tree of configurations.
//
// This Tree structure can be used to get (download) new modules, load
// all the modules without getting, flatten the tree into something
// Terraform can use, etc.
type Tree struct {
	name     string
	config   *config.Config
	children map[string]*Tree
	lock     sync.RWMutex
}

// GetMode is an enum that describes how modules are loaded.
//
// GetModeLoad says that modules will not be downloaded or updated, they will
// only be loaded from the storage.
//
// GetModeGet says that modules can be initially downloaded if they don't
// exist, but otherwise to just load from the current version in storage.
//
// GetModeUpdate says that modules should be checked for updates and
// downloaded prior to loading. If there are no updates, we load the version
// from disk, otherwise we download first and then load.
type GetMode byte

const (
	GetModeNone GetMode = iota
	GetModeGet
	GetModeUpdate
)

// NewTree returns a new Tree for the given config structure.
func NewTree(name string, c *config.Config) *Tree {
	return &Tree{config: c, name: name}
}

// NewTreeModule is like NewTree except it parses the configuration in
// the directory and gives it a specific name. Use a blank name "" to specify
// the root module.
func NewTreeModule(name, dir string) (*Tree, error) {
	c, err := config.LoadDir(dir)
	if err != nil {
		return nil, err
	}

	return NewTree(name, c), nil
}

// Children returns the children of this tree (the modules that are
// imported by this root).
//
// This will only return a non-nil value after Load is called.
func (t *Tree) Children() map[string]*Tree {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.children
}

// Loaded says whether or not this tree has been loaded or not yet.
func (t *Tree) Loaded() bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.children != nil
}

// Modules returns the list of modules that this tree imports.
//
// This is only the imports of _this_ level of the tree. To retrieve the
// full nested imports, you'll have to traverse the tree.
func (t *Tree) Modules() []*Module {
	result := make([]*Module, len(t.config.Modules))
	for i, m := range t.config.Modules {
		result[i] = &Module{
			Name:   m.Name,
			Source: m.Source,
		}
	}

	return result
}

// Name returns the name of the tree. This will be "<root>" for the root
// tree and then the module name given for any children.
func (t *Tree) Name() string {
	if t.name == "" {
		return "<root>"
	}

	return t.name
}

// Load loads the configuration of the entire tree.
//
// The parameters are used to tell the tree where to find modules and
// whether it can download/update modules along the way.
//
// Calling this multiple times will reload the tree.
//
// Various semantic-like checks are made along the way of loading since
// module trees inherently require the configuration to be in a reasonably
// sane state: no circular dependencies, proper module sources, etc. A full
// suite of validations can be done by running Validate (after loading).
func (t *Tree) Load(s Storage, mode GetMode) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	// Reset the children if we have any
	t.children = nil

	modules := t.Modules()
	children := make(map[string]*Tree)

	// Go through all the modules and get the directory for them.
	update := mode == GetModeUpdate
	for _, m := range modules {
		if _, ok := children[m.Name]; ok {
			return fmt.Errorf(
				"module %s: duplicated. module names must be unique", m.Name)
		}

		source, err := Detect(m.Source, t.config.Dir)
		if err != nil {
			return fmt.Errorf("module %s: %s", m.Name, err)
		}

		if mode > GetModeNone {
			// Get the module since we specified we should
			if err := s.Get(source, update); err != nil {
				return err
			}
		}

		// Get the directory where this module is so we can load it
		dir, ok, err := s.Dir(source)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf(
				"module %s: not found, may need to be downloaded", m.Name)
		}

		// Load the configuration
		children[m.Name], err = NewTreeModule(m.Name, dir)
		if err != nil {
			return fmt.Errorf(
				"module %s: %s", m.Name, err)
		}
	}

	// Go through all the children and load them.
	for _, c := range children {
		if err := c.Load(s, mode); err != nil {
			return err
		}
	}

	// Set our tree up
	t.children = children

	return nil
}

// String gives a nice output to describe the tree.
func (t *Tree) String() string {
	var result bytes.Buffer
	result.WriteString(t.Name() + "\n")

	cs := t.Children()
	if cs == nil {
		result.WriteString("  not loaded")
	} else {
		// Go through each child and get its string value, then indent it
		// by two.
		for _, c := range cs {
			r := strings.NewReader(c.String())
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				result.WriteString("  ")
				result.WriteString(scanner.Text())
				result.WriteString("\n")
			}
		}
	}

	return result.String()
}

// Validate does semantic checks on the entire tree of configurations.
//
// This will call the respective config.Config.Validate() functions as well
// as verifying things such as parameters/outputs between the various modules.
//
// Load must be called prior to calling Validate or an error will be returned.
func (t *Tree) Validate() error {
	if !t.Loaded() {
		return fmt.Errorf("tree must be loaded before calling Validate")
	}

	// If something goes wrong, here is our error template
	newErr := &TreeError{Name: []string{t.Name()}}

	// Validate our configuration first.
	if err := t.config.Validate(); err != nil {
		newErr.Err = err
		return newErr
	}

	// Get the child trees
	children := t.Children()

	// Validate all our children
	for _, c := range children {
		err := c.Validate()
		if err == nil {
			continue
		}

		verr, ok := err.(*TreeError)
		if !ok {
			// Unknown error, just return...
			return err
		}

		// Append ourselves to the error and then return
		verr.Name = append(verr.Name, t.Name())
		return verr
	}

	// Go over all the modules and verify that any parameters are valid
	// variables into the module in question.
	for _, m := range t.config.Modules {
		tree, ok := children[m.Name]
		if !ok {
			// This should never happen because Load watches us
			panic("module not found in children: " + m.Name)
		}

		// Build the variables that the module defines
		varMap := make(map[string]struct{})
		for _, v := range tree.config.Variables {
			varMap[v.Name] = struct{}{}
		}

		// Compare to the keys in our raw config for the module
		for k, _ := range m.RawConfig.Raw {
			if _, ok := varMap[k]; !ok {
				newErr.Err = fmt.Errorf(
					"module %s: %s is not a valid parameter",
					m.Name, k)
				return newErr
			}
		}
	}

	// Go over all the variables used and make sure that any module
	// variables represent outputs properly.
	for source, vs := range t.config.InterpolatedVariables() {
		for _, v := range vs {
			mv, ok := v.(*config.ModuleVariable)
			if !ok {
				continue
			}

			tree, ok := children[mv.Name]
			if !ok {
				// This should never happen because Load watches us
				panic("module not found in children: " + mv.Name)
			}

			found := false
			for _, o := range tree.config.Outputs {
				if o.Name == mv.Field {
					found = true
					break
				}
			}
			if !found {
				newErr.Err = fmt.Errorf(
					"%s: %s is not a valid output for module %s",
					source, mv.Field, mv.Name)
				return newErr
			}
		}
	}

	return nil
}

// TreeError is an error returned by Tree.Validate if an error occurs
// with validation.
type TreeError struct {
	Name []string
	Err  error
}

func (e *TreeError) Error() string {
	// Build up the name
	var buf bytes.Buffer
	for _, n := range e.Name {
		buf.WriteString(n)
		buf.WriteString(".")
	}
	buf.Truncate(buf.Len() - 1)

	// Format the value
	return fmt.Sprintf("module %s: %s", buf.String(), e.Err)
}
