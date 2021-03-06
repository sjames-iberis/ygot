// Copyright 2019 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package genutil provides utility functions for package that generate code
// based on a YANG schema.
package genutil

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/openconfig/goyang/pkg/yang"
	"github.com/openconfig/ygot/util"
	"github.com/openconfig/ygot/ygot"
)

const (
	// GoDefaultYgotImportPath is the default import path used for the ygot library
	// in the generated code.
	GoDefaultYgotImportPath string = "github.com/openconfig/ygot/ygot"
	// GoDefaultYtypesImportPath is the default import path used for the ytypes library
	// in the generated code.
	GoDefaultYtypesImportPath string = "github.com/openconfig/ygot/ytypes"
	// GoDefaultGoyangImportPath is the default path for the goyang/pkg/yang library that
	// is used in the generated code.
	GoDefaultGoyangImportPath string = "github.com/openconfig/goyang/pkg/yang"
	// GoDefaultGNMIImportPath is the default import path that is used for the gNMI generated
	// Go protobuf code in the generated output.
	GoDefaultGNMIImportPath string = "github.com/openconfig/gnmi/proto/gnmi"
)

// WriteIfNotEmpty writes the string s to b if it has a non-zero length.
func WriteIfNotEmpty(b *bytes.Buffer, s string) {
	if len(s) != 0 {
		b.WriteString(s)
	}
}

// TypeDefaultValue returns the default value of the type t if it is specified.
// nil is returned if no default is specified.
func TypeDefaultValue(t *yang.YangType) *string {
	if t == nil || t.Default == "" {
		return nil
	}
	return ygot.String(t.Default)
}

// GetOrderedEntryKeys returns the keys of a map of *yang.Entry in alphabetical order.
func GetOrderedEntryKeys(entries map[string]*yang.Entry) []string {
	var orderedKeys []string
	for key := range entries {
		orderedKeys = append(orderedKeys, key)
	}
	sort.Strings(orderedKeys)
	return orderedKeys
}

// findAllChildrenWithoutCompression finds the entries that are children of an
// entry e, when not compressing paths. It does not recurse into any child nodes
// other than those that do not represent data tree nodes (i.e., choice and
// case nodes). Choice and case nodes themselves are not appended to the children
// list. If the excludeState argument is set to true, children that are
// config false (i.e., read only) in the YANG schema are not returned.
func findAllChildrenWithoutCompression(e *yang.Entry, excludeState bool) (map[string]*yang.Entry, []error) {
	var errs []error
	directChildren := map[string]*yang.Entry{}
	for _, child := range util.Children(e) {
		// Exclude children that are config false if requested.
		if excludeState && !util.IsConfig(child) {
			continue
		}

		// For each child, if it is a case or choice, then find the set of nodes that
		// are not choice or case nodes and append them to the directChildren map,
		// so they are effectively skipped over.
		if util.IsChoiceOrCase(child) {
			errs = addNonChoiceChildren(directChildren, child, errs)
		} else {
			errs = addNewChild(directChildren, child.Name, child, errs)
		}
	}
	return directChildren, errs
}

// CompressBehaviour specifies how the set of direct children of any entry should determined.
// Compression indicates whether paths should be compressed in the output of an
// OpenConfig schema; however, there are different ways of compressing nodes.
type CompressBehaviour int64

// Why use an enum?
// There are 3 dimensions here: compress|preferState|excludeDerivedState
// No dimension spans across all others' options, so can't extract any out
// without having to error out for some combinations.
const (
	// Uncompressed means to not compress the schema.
	Uncompressed CompressBehaviour = iota
	// UncompressedExcludeDerivedState excludes config false subtrees.
	UncompressedExcludeDerivedState
	// PreferIntendedConfig indicates to use the "config" version of a
	// schema entry instead of its "state" counterpart when both exist.
	PreferIntendedConfig
	// PreferOperationalState indicates to use the "state" version of a
	// schema entry instead of its "config" counterpart when both exist.
	PreferOperationalState // prefer applied config
	// ExcludeDerivedState excludes all values that are not writeable
	// (i.e. config false), including their children, from the generated
	// code output.
	ExcludeDerivedState
)

// CompressEnabled is a helper to query whether compression is on.
func (c CompressBehaviour) CompressEnabled() bool {
	switch c {
	case Uncompressed, UncompressedExcludeDerivedState:
		return false
	}
	return true
}

// StateExcluded is a helper to query whether derived state is excluded.
func (c CompressBehaviour) StateExcluded() bool {
	switch c {
	case ExcludeDerivedState, UncompressedExcludeDerivedState:
		return true
	}
	return false
}

// TranslateToCompressBehaviour translates the set of (compressPaths,
// excludeState) into a subset of CompressBehaviour options.
// TODO(wenbli:b/142679709): This serves as a workaround before generator
// scripts need to change to make use of the unused options.
func TranslateToCompressBehaviour(compressPaths, excludeState bool) CompressBehaviour {
	switch {
	case compressPaths && excludeState:
		return ExcludeDerivedState
	case compressPaths:
		return PreferIntendedConfig
	case excludeState:
		return UncompressedExcludeDerivedState
	default:
		return Uncompressed
	}
}

// FindAllChildren finds the data tree elements that are children of a YANG entry e, which
// should have code generated for them. In general, this means data tree elements that are
// directly connected to a particular data tree element; however, when compression of the
// schema is enabled then recursion is required.
//
// For example, if we have a YANG tree:
//    /interface (list)
//    /interface/config (container)
//    /interface/config/admin-state (leaf)
//    /interface/state (container)
//    /interface/state/admin-state (leaf)
//    /interface/state/oper-state (leaf)
//    /interface/state/counters (container)
//    /interface/state/counters/in-pkts (leaf)
//    /interface/ethernet/config (container)
//    /interface/ethernet/config/mac-address (leaf)
//    /interface/ethernet/state (state)
//    /interface/ethernet/state/mac-address (leaf)
//    /interface/subinterfaces (container)
//    /interface/subinterfaces/subinterface (list)
//
// With compression disabled, then each directly connected child of a container should have
// code generated for it - so therefore we end up with:
//
//    /interface: config, state, ethernet, subinterfaces
//    /interface/config: admin-state
//    /interface/state: admin-state, oper-state, counters
//    /interface/state/counters: in-pkts
//    /interface/ethernet: config, state
//    /interface/ethernet/config: mac-address
//    /interface/ethernet/state: mac-address
//    /interface/subinterfaces: subinterface
//
// This is simply achieved by examining the directory provided by goyang (e.Dir)
// and extracting the direct children that exist. These are appended to the directChildren
// map (keyed on element name) and then returned.
//
// When compression is on, then more complex logic is required based on the OpenConfig path
// rules. In this case, the following "look-aheads" are implemented:
//
//  1. The 'config' and 'state' containers under a directory are removed. This is because
//  OpenConfig duplicates nodes under config and state to represent intended versus applied
//  configuration. In the compressed schema then we need to drop one of these configuration
//  leaves (those leaves that are defined as the set under the 'state' container that also
//  exist within the 'config' container), and compressBehaviour specifies which one to drop.
//  The logic implemented is to recurse into the config container, and select these leaves as
//  direct children of the original parent. Any leaves that do not exist in the 'config'
//  container but do within 'state' are operation state leaves, and hence are also mapped.
//
//     Above, this means that /interfaces/interface has the admin-state and oper-state as direct
//     children.
//
//     Since containers can exist under the 'state' container, then these containers are also
//     considered as direct children of e.
//
//  2. Surrounding containers for lists are removed - that is to say, in an OpenConfig schema
//     a list (e.g. /interface/subinterfaces/subinterface) always has a container that surrounds
//     it. This is due to implementation requirements when it is supported on vendor devices.
//     However, to a developer this looks like stuttering, and hence we remove this - by checking
//     that for each directory that would be a child of e, if it has only one child, which is
//     a list, then we skip over it.
//
// Implementing these two rules means that the schema is simplified, such that the tree described
// becomes:
//
//	/interface: admin-state, oper-state, counters, ethernet, subinterface
//	/interface/counters: in-pkts
//	/interface/ethernet: mac-address
//
// As can be seen the advantage of this compression is that the set of entities for which code
// generation is done is smaller, with less levels of schema hierarchy. However, it depends upon
// a number of rules of the OpenConfig schema. If compression is on but the schema
// does not comply with the rules of OpenConfig schema, then errors may occur and be returned
// in the []error slice by findAllChildren.
//
// It should be noted that special handling is required for choice and case - because these are
// directories within the resulting schema, but they are not data tree nodes. So for example,
// we can have:
//	/container/choice/case-one/leaf-a
//	/container/choice/case-two/leaf-b
// In this tree, "choice" and "case-one" (which are choice and case nodes respectively) are not
// valid data tree elements, so we recurse down both of the branches of "choice" to return leaf-a
// and leaf-b. Since choices can be nested (/choice-a/choice-b/choice-c/case-a), and can have
// multiple data nodes per case, then the addNonChoiceChildren function will find the first
// children of the specified node that are not choice or case statements themselves (i.e., leaf-a
// and leaf-b in the above example).
//
// The .*ExcludeDerivedState compress behaviour options further filters the returned set of
// children based on their YANG 'config' status. When set, then
// any read-only (config false) node is excluded from the returned set of children.
// The 'config' status is inherited from a entry's parent if required, as per
// the rules in RFC6020.
func FindAllChildren(e *yang.Entry, compBehaviour CompressBehaviour) (map[string]*yang.Entry, []error) {
	excludeState := compBehaviour == ExcludeDerivedState || compBehaviour == UncompressedExcludeDerivedState
	// If we are asked to exclude 'config false' leaves, and this node is
	// config false itself, then we can return an empty set of children since
	// config false is inherited from the parent by all children.
	if excludeState && !util.IsConfig(e) {
		return nil, nil
	}

	var prioData, deprioData string
	switch compBehaviour {
	case Uncompressed, UncompressedExcludeDerivedState:
		// If compression is not required, then we do not need to recurse into as many
		// nodes, so return simply the first level direct children (other than choice or case).
		return findAllChildrenWithoutCompression(e, excludeState)
	case PreferIntendedConfig, ExcludeDerivedState:
		prioData, deprioData = "config", "state"
	case PreferOperationalState:
		prioData, deprioData = "state", "config"
	}

	// orderedChildNames is used to provide an ordered list of the name of children
	// to check.
	var orderedChildNames []string

	// If this is a directory and it has a container named "config"/"state"
	// underneath it then we must process the prioritized one first. This
	// is due to the fact that in the schema there are duplicated leaves
	// under config/ and state/ - and we want to provide the prioritized
	// one to the mapping code. This is important as we care about the path
	// that is handed to code that subsequently maps back to the
	// uncompressed schema.
	//
	// To achieve this then we build an orderedChildNames slice which specifies the
	// order in which we should process the children of entry e.
	if e.IsContainer() || e.IsList() {
		if _, ok := e.Dir[prioData]; ok {
			orderedChildNames = append(orderedChildNames, prioData)
		}
	}

	// We now append all other entries in the directory to the orderedChildren list.
	for _, child := range util.Children(e) {
		if child.Name != prioData {
			orderedChildNames = append(orderedChildNames, child.Name)
		}
	}

	// Errors encountered during the extraction of the elements that should
	// be direct children of the entity representing e.
	var errs []error
	// prioNames is the set of names under the prioritized data container
	// that are added as children. This is a whitelist for any duplicate
	// names in the deprioritized data container.
	prioNames := map[string]bool{}
	// directChildren is used to store the nodes that will be mapped to be direct
	// children of the struct that represents the entry e being processed. It is
	// keyed by the name of the child YANG node ((yang.Entry).Name).
	directChildren := make(map[string]*yang.Entry)
	for _, currChild := range orderedChildNames {
		switch {
		// If config false values are being excluded, and this child is config
		// false, then simply skip it from being considered. This check is performed
		// first to avoid comparisons on this node which are irrelevant.
		case excludeState && !util.IsConfig(e.Dir[currChild]):
			continue
			// Implement rule 1 from the function documentation - skip over config and state
			// containers.
		case util.IsConfigState(e.Dir[currChild]):
			// Recurse into this directory so that we extract its children and
			// present them as being at a higher-layer. This allows the "config"
			// and "state" container to be removed from the schema.
			// For example, /foo/bar/config/{a,b,c} becomes /foo/bar/{a,b,c}.
			for _, configStateChild := range util.Children(e.Dir[currChild]) {
				// If we get an error for the deprioritized data container then we ignore it as we
				// expect that there are some duplicates here for applied configuration leaves
				// (those that appear both in the "config" and "state" container).
				if e.Dir[currChild].Name == deprioData {
					// Compress out (do not map) choice/case nodes that are in the
					// config or state container. This is again specifically for the
					// OpenConfig routing policy model.
					// Further, if the name is a duplicate to one that's already in the
					// prioritized container, we must drop the entry, and ignore any error
					// that is returned, as we allow those duplicates.
					if util.IsChoiceOrCase(configStateChild) {
						// Duplicates could occur in a choice/case as well.
						errs = addNonChoiceChildrenDuplist(directChildren, configStateChild, prioNames, errs)
					} else if !prioNames[configStateChild.Name] {
						errs = addNewChild(directChildren, configStateChild.Name, configStateChild, errs)
					}
				} else {
					// Handle the specific case of having a choice underneath a config
					// or state container as this occurs in the routing policy model.
					if util.IsChoiceOrCase(configStateChild) {
						errs = addNonChoiceChildren(directChildren, configStateChild, errs)
					} else {
						errs = addNewChild(directChildren, configStateChild.Name, configStateChild, errs)
					}
				}
				// If this is the prioritized data container, add the names to the
				// whitelist. When processing nodes under the deprioritized data container,
				// we will tolerate duplication of any names in this set, but not any other
				// names.
				if e.Dir[currChild].Name == prioData {
					if util.IsChoiceOrCase(configStateChild) {
						for _, entry := range util.FindFirstNonChoiceOrCase(configStateChild) {
							prioNames[entry.Name] = true
						}
					} else {
						prioNames[configStateChild.Name] = true
					}
				}
			}
		case e.Dir[currChild].IsDir():
			// This is a directory that is not a config or state directory, so it is
			// either purely hierarchical or a surrounding container for a list.
			///
			// e.Dir[currChild] is the first level child of the container that we're looking at
			// which is any directory in the YANG schema that is not a "config" or
			// "state" container, as well as choice/case nodes, since these also
			// contain child nodes.
			//
			// eGrandChildren is a slice of the elements that are children of the
			// directory that was a child of e.
			eGrandChildren := util.Children(e.Dir[currChild])
			switch {
			// Implement rule 2 - remove surrounding containers for lists and consider
			// the list under the surrounding container a direct child.
			case len(eGrandChildren) == 1 && eGrandChildren[0].IsList():
				if !util.IsConfig(eGrandChildren[0]) && excludeState {
					// If the list child is read-only, then it is not a valid child.
					continue
				}
				errs = addNewChild(directChildren, eGrandChildren[0].Name, eGrandChildren[0], errs)
				// See note in function documentation about choice and case nodes - which are
				// not valid data tree elements. We therefore skip past any number of nested
				// choice/case statements and treat the first data tree elements as direct children.
			case util.IsChoiceOrCase(e.Dir[currChild]):
				errs = addNonChoiceChildren(directChildren, e.Dir[currChild], errs)
			default:
				// This is simply a normal container so map it into the hierarchy
				// as a direct child.
				errs = addNewChild(directChildren, e.Dir[currChild].Name, e.Dir[currChild], errs)
			}
		default:
			// This is a leaf node - but we want to ignore leafref nodes that are
			// within a list because these are duplicated keys.
			if !(e.IsList() && e.Dir[currChild].Type.Kind == yang.Yleafref) {
				errs = addNewChild(directChildren, e.Dir[currChild].Name, e.Dir[currChild], errs)
			}
		}
	}
	return directChildren, errs
}

// addNonChoiceChildren recurses into a yang.entry e and finds the first
// nodes that are neither choice nor case nodes. It appends these to the map of
// yang.Entry nodes specified by m. If errors are encountered when adding an
// element, an error is appended to the errs slice, which is returned by the
// function.
func addNonChoiceChildren(m map[string]*yang.Entry, e *yang.Entry, errs []error) []error {
	nch := util.FindFirstNonChoiceOrCase(e)
	for _, n := range nch {
		errs = addNewChild(m, n.Name, n, errs)
	}
	return errs
}

// addNonChoiceChildrenDupWhitelist recurses into a yang.entry e and finds the first
// nodes that are neither choice nor case nodes. It appends these to the map of
// yang.Entry nodes specified by m. If errors are encountered when adding an
// element, an error is appended to the errs slice, which is returned by the
// function. duplist is a whitelist where duplicate names that hit this list
// are not counted as errors, and simply skipped.
func addNonChoiceChildrenDuplist(m map[string]*yang.Entry, e *yang.Entry, duplist map[string]bool, errs []error) []error {
	nch := util.FindFirstNonChoiceOrCase(e)
	for _, n := range nch {
		// Duplicates in the duplist are expected.
		if duplist[n.Name] {
			continue
		}
		errs = addNewChild(m, n.Name, n, errs)
	}
	return errs
}

// addNewChild adds a new key (k) to a map with value v if k is not already
// defined in the map. When the key k is defined in the map an error is appended
// to errs, which is subsequently returned.
func addNewChild(m map[string]*yang.Entry, k string, v *yang.Entry, errs []error) []error {
	if _, ok := m[k]; !ok {
		m[k] = v
		return errs
	}
	errs = append(errs, fmt.Errorf("%s was duplicate", v.Path()))
	return errs
}
