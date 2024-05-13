// nolint
package extramanifest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	apiRes "k8s.io/apimachinery/pkg/api/resource"
)

// Code copied from the config policy controller for merging manifests the ACM way:
// https://github.com/open-cluster-management-io/config-policy-controller/blob/d734b7aa1ec2ae936edb48ec622b29ac70871921/controllers/configurationpolicy_controller.go#L2168
// https://github.com/open-cluster-management-io/config-policy-controller/blob/76ca0cd30c2bcd8a965ab25ab00dc41094afdb9b/controllers/configurationpolicy_utils.go#L150
// To be removed when these functions can be imported

// mergeSpecs is a wrapper for the recursive function to merge 2 maps.
func mergeSpecs(templateVal, existingVal interface{}, ctype string, zeroValueEqualsNil bool) (interface{}, error) {
	// Copy templateVal since it will be modified in mergeSpecsHelper
	data1, err := json.Marshal(templateVal)
	if err != nil {
		return nil, err
	}

	var j1 interface{}

	err = json.Unmarshal(data1, &j1)
	if err != nil {
		return nil, err
	}

	return mergeSpecsHelper(j1, existingVal, ctype, zeroValueEqualsNil), nil
}

// mergeSpecsHelper is a helper function that takes an object from the existing object and merges in
// all the data that is different in the template. This way, comparing the merged object to the one
// that exists on the cluster will tell you whether the existing object is compliant with the template.
// This function uses recursion to check mismatches in nested objects and is the basis for most
// comparisons the controller makes.
func mergeSpecsHelper(templateVal, existingVal interface{}, ctype string, zeroValueEqualsNil bool) interface{} {
	switch templateVal := templateVal.(type) {
	case map[string]interface{}:
		existingVal, ok := existingVal.(map[string]interface{})
		if !ok {
			// if one field is a map and the other isn't, don't bother merging -
			// just returning the template value will still generate noncompliant
			return templateVal
		}
		// otherwise, iterate through all fields in the template object and
		// merge in missing values from the existing object
		for k, v2 := range existingVal {
			if v1, ok := templateVal[k]; ok {
				templateVal[k] = mergeSpecsHelper(v1, v2, ctype, zeroValueEqualsNil)
			} else {
				templateVal[k] = v2
			}
		}
	case []interface{}: // list nested in map
		existingVal, ok := existingVal.([]interface{})
		if !ok {
			// if one field is a list and the other isn't, don't bother merging
			return templateVal
		}

		if len(existingVal) > 0 {
			// if both values are non-empty lists, we need to merge in the extra data in the existing
			// object to do a proper compare
			return mergeArrays(templateVal, existingVal, ctype, zeroValueEqualsNil)
		}
	case nil:
		// if template value is nil, pull data from existing, since the template does not care about it
		existingVal, ok := existingVal.(map[string]interface{})
		if ok {
			return existingVal
		}
	}

	_, ok := templateVal.(string)
	if !ok {
		return templateVal
	}

	return templateVal.(string)
}

type countedVal struct {
	value interface{}
	count int
}

// mergeArrays is a helper function that takes a list from the existing object and merges in all the data that is
// different in the template.
func mergeArrays(
	desiredArr []interface{}, existingArr []interface{}, ctype string, zeroValueEqualsNil bool,
) (result []interface{}) {
	if ctype == "mustonlyhave" {
		return desiredArr
	}

	desiredArrCopy := append([]interface{}{}, desiredArr...)
	idxWritten := map[int]bool{}

	for i := range desiredArrCopy {
		idxWritten[i] = false
	}

	// create a set with a key for each unique item in the list
	oldItemSet := make(map[string]*countedVal)

	for _, val2 := range existingArr {
		key := fmt.Sprint(val2)

		if entry, ok := oldItemSet[key]; ok {
			entry.count++
		} else {
			oldItemSet[key] = &countedVal{value: val2, count: 1}
		}
	}

	seen := map[string]bool{}

	// Iterate both arrays in order to favor the case when the object is already compliant.
	for _, val2 := range existingArr {
		key := fmt.Sprint(val2)
		if seen[key] {
			continue
		}

		seen[key] = true

		count := 0
		val2 := oldItemSet[key].value
		// for each list item in the existing array, iterate through the template array and try to find a match
		for desiredArrIdx, val1 := range desiredArrCopy {
			if idxWritten[desiredArrIdx] {
				continue
			}

			var mergedObj interface{}
			// Stores if val1 and val2 are maps with the same "name" key value. In the case of the containers array
			// in a Deployment object, the value should be merged and not appended if the name is the same in both.
			var sameNamedObjects bool

			switch val2 := val2.(type) {
			case map[string]interface{}:
				// If the policy value and the current value are different types, use the same logic
				// as the default case.
				val1, ok := val1.(map[string]interface{})
				if !ok {
					mergedObj = val1

					break
				}

				if name2, ok := val2["name"].(string); ok && name2 != "" {
					if name1, ok := val1["name"].(string); ok && name1 == name2 {
						sameNamedObjects = true
					}
				}

				// use map compare helper function to check equality on lists of maps
				mergedObj, _ = compareSpecs(val1, val2, ctype, zeroValueEqualsNil)
			default:
				mergedObj = val1
			}
			// if a match is found, this field is already in the template, so we can skip it in future checks
			if sameNamedObjects || equalObjWithSort(mergedObj, val2, zeroValueEqualsNil) {
				count++

				desiredArr[desiredArrIdx] = mergedObj
				idxWritten[desiredArrIdx] = true
			}

			// If the result of merging val1 (template) into val2 (existing value) matched val2 for the required count,
			// move on to the next existing value.
			if count == oldItemSet[key].count {
				break
			}
		}
		// if an item in the existing object cannot be found in the template, we add it to the template array
		// to produce the merged array
		if count < oldItemSet[key].count {
			for i := 0; i < (oldItemSet[key].count - count); i++ {
				desiredArr = append(desiredArr, val2)
			}
		}
	}

	return desiredArr
}

// compareSpecs is a wrapper function that creates a merged map for mustHave
// and returns the template map for mustonlyhave
func compareSpecs(
	newSpec, oldSpec map[string]interface{}, ctype string, zeroValueEqualsNil bool,
) (updatedSpec map[string]interface{}, err error) {
	if ctype == "mustonlyhave" {
		return newSpec, nil
	}
	// if compliance type is musthave, create merged object to compare on
	merged, err := mergeSpecs(newSpec, oldSpec, ctype, zeroValueEqualsNil)
	if err != nil {
		return merged.(map[string]interface{}), err
	}

	return merged.(map[string]interface{}), nil
}

// equalObjWithSort is a wrapper function that calls the correct function to check equality depending on what
// type the objects to compare are
func equalObjWithSort(mergedObj interface{}, oldObj interface{}, zeroValueEqualsNil bool) (areEqual bool) {
	switch mergedObj := mergedObj.(type) {
	case map[string]interface{}:
		if oldObjMap, ok := oldObj.(map[string]interface{}); ok {
			return checkFieldsWithSort(mergedObj, oldObjMap, zeroValueEqualsNil)
		}
		// this includes the case where oldObj is nil
		return false
	case []interface{}:
		if len(mergedObj) == 0 && oldObj == nil {
			return true
		}

		if oldObjList, ok := oldObj.([]interface{}); ok {
			return checkListsMatch(mergedObj, oldObjList)
		}

		return false
	default: // when mergedObj's type is string, int, bool, or nil
		if zeroValueEqualsNil {
			if oldObj == nil && mergedObj != nil {
				// compare the zero value of mergedObj's type to mergedObj
				ref := reflect.ValueOf(mergedObj)
				zero := reflect.Zero(ref.Type()).Interface()

				return fmt.Sprint(zero) == fmt.Sprint(mergedObj)
			}

			if mergedObj == nil && oldObj != nil {
				// compare the zero value of oldObj's type to oldObj
				ref := reflect.ValueOf(oldObj)
				zero := reflect.Zero(ref.Type()).Interface()

				return fmt.Sprint(zero) == fmt.Sprint(oldObj)
			}
		}

		return fmt.Sprint(mergedObj) == fmt.Sprint(oldObj)
	}
}

// checkListsMatch is a generic list check that uses an arbitrary sort to ensure it is comparing the right values
func checkListsMatch(oldVal []interface{}, mergedVal []interface{}) (m bool) {
	if (oldVal == nil && mergedVal != nil) || (oldVal != nil && mergedVal == nil) {
		return false
	}

	if len(mergedVal) != len(oldVal) {
		return false
	}

	// Make copies of the lists, so we can sort them without mutating this function's inputs
	oVal := append([]interface{}{}, oldVal...)
	mVal := append([]interface{}{}, mergedVal...)

	sort.Slice(oVal, func(i, j int) bool {
		return sortAndSprint(oVal[i]) < sortAndSprint(oVal[j])
	})
	sort.Slice(mVal, func(x, y int) bool {
		return sortAndSprint(mVal[x]) < sortAndSprint(mVal[y])
	})

	for idx, oNestedVal := range oVal {
		switch oNestedVal := oNestedVal.(type) {
		case map[string]interface{}:
			// if list contains maps, recurse on those maps to check for a match
			if mVal, ok := mVal[idx].(map[string]interface{}); ok {
				if !checkFieldsWithSort(mVal, oNestedVal, true) {
					return false
				}

				continue
			}

			return false
		default:
			// otherwise, just do a generic check
			if fmt.Sprint(oNestedVal) != fmt.Sprint(mVal[idx]) {
				return false
			}
		}
	}

	return true
}

// checkFieldsWithSort is a check for maps that uses an arbitrary sort to ensure it is
// comparing the right values
func checkFieldsWithSort(
	mergedObj map[string]interface{}, oldObj map[string]interface{}, zeroValueEqualsNil bool,
) (matches bool) {
	// needed to compare lists, since merge messes up the order
	if len(mergedObj) < len(oldObj) {
		return false
	}

	for i, mVal := range mergedObj {
		switch mVal := mVal.(type) {
		case map[string]interface{}:
			// if field is a map, recurse to check for a match
			oVal, ok := oldObj[i].(map[string]interface{})
			if !ok {
				if zeroValueEqualsNil && len(mVal) == 0 {
					break
				}

				return false
			}

			if !checkFieldsWithSort(mVal, oVal, zeroValueEqualsNil) {
				return false
			}
		case []interface{}:
			// if field is a generic list, sort and iterate through them to make sure each value matches
			oVal, ok := oldObj[i].([]interface{})
			if !ok {
				if len(mVal) == 0 {
					break
				}

				return false
			}

			if len(mVal) != len(oVal) || !checkListsMatch(oVal, mVal) {
				return false
			}
		case string:
			// extra check to see if value is a byte value
			mQty, err := apiRes.ParseQuantity(mVal)
			if err != nil {
				oVal, ok := oldObj[i]
				if !ok {
					return false
				}

				// An error indicates the value is a regular string, so check equality normally
				if fmt.Sprint(oVal) != fmt.Sprint(mVal) {
					return false
				}
			} else {
				// if the value is a quantity of bytes, convert original
				oVal, ok := oldObj[i].(string)
				if !ok {
					return false
				}

				oQty, err := apiRes.ParseQuantity(oVal)
				if err != nil || !oQty.Equal(mQty) {
					return false
				}
			}
		default:
			// if field is not an object, just do a basic compare to check for a match
			oVal := oldObj[i]
			// When oVal value omitted because of omitempty
			if oVal == nil && mVal != nil {
				ref := reflect.ValueOf(mVal)
				oVal = reflect.Zero(ref.Type()).Interface()
			}

			if fmt.Sprint(oVal) != fmt.Sprint(mVal) {
				return false
			}
		}
	}

	return true
}

// sortAndSprint sorts any lists in the input, and formats the resulting object as a string
func sortAndSprint(item interface{}) string {
	switch item := item.(type) {
	case map[string]interface{}:
		sorted := make(map[string]string, len(item))

		for key, val := range item {
			sorted[key] = sortAndSprint(val)
		}

		return fmt.Sprintf("%v", sorted)
	case []interface{}:
		sorted := make([]string, len(item))

		for i, val := range item {
			sorted[i] = sortAndSprint(val)
		}

		sort.Slice(sorted, func(x, y int) bool {
			return sorted[x] < sorted[y]
		})

		return fmt.Sprintf("%v", sorted)
	default:
		return fmt.Sprintf("%v", item)
	}
}
