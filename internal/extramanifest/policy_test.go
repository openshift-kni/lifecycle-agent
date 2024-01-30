package extramanifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	policiesv1 "open-cluster-management.io/governance-policy-propagator/api/v1"
)

func createDummyPolicyWithName(name string) *policiesv1.Policy {
	return &policiesv1.Policy{
		ObjectMeta: v1.ObjectMeta{Name: name},
	}
}

func TestControllerSortMapByValue(t *testing.T) {

	abcdef := createDummyPolicyWithName("abcdef")
	bcd := createDummyPolicyWithName("bcd")
	acd := createDummyPolicyWithName("acd")
	efg := createDummyPolicyWithName("efg")
	cdef := createDummyPolicyWithName("cdef")
	abcdehhh := createDummyPolicyWithName("abcdehhh")

	testcases := []struct {
		inputMap       map[*policiesv1.Policy]int
		expectedResult []*policiesv1.Policy
	}{
		{
			inputMap:       map[*policiesv1.Policy]int{abcdef: 20, bcd: -1, acd: 10, efg: 2, cdef: 100},
			expectedResult: []*policiesv1.Policy{bcd, efg, acd, abcdef, cdef},
		},
		{
			inputMap:       map[*policiesv1.Policy]int{abcdef: 20, bcd: -2, acd: 8, efg: 20, cdef: 20},
			expectedResult: []*policiesv1.Policy{bcd, acd, abcdef, cdef, efg},
		},
		{
			inputMap:       map[*policiesv1.Policy]int{abcdef: 20, bcd: 8, acd: 8, efg: 20, cdef: 1},
			expectedResult: []*policiesv1.Policy{cdef, acd, bcd, abcdef, efg},
		},
		{
			inputMap:       map[*policiesv1.Policy]int{abcdef: 20, bcd: 8, acd: 0, abcdehhh: 20, cdef: 8},
			expectedResult: []*policiesv1.Policy{acd, bcd, cdef, abcdef, abcdehhh},
		},
	}

	for _, tc := range testcases {
		result := sortPolicyMap(tc.inputMap)
		assert.Equal(t, result, tc.expectedResult)
	}
}

func TestGetParentPolicyNameAndNamespace(t *testing.T) {
	res, err := getParentPolicyNameAndNamespace("default.upgrade")
	assert.NoError(t, err)
	assert.Equal(t, len(res), 2)

	res, err = getParentPolicyNameAndNamespace("upgrade")
	assert.Error(t, err)

	res, err = getParentPolicyNameAndNamespace("default.upgrade.cluster")
	assert.Equal(t, res[0], "default")
	assert.Equal(t, res[1], "upgrade.cluster")
}
