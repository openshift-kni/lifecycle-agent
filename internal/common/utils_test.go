package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRemoveDuplicates(t *testing.T) {
	ints := []int{3, 1, 1, 2}
	res := RemoveDuplicates[int](ints)
	assert.Equal(t, []int{3, 1, 2}, res)

	ints = []int{}
	res = RemoveDuplicates[int](ints)
	assert.Equal(t, []int{}, res)

	strs := []string{"a/b/c/d", "a/b/c", "a/b/c", "a/b/c/d"}
	resStr := RemoveDuplicates[string](strs)
	assert.Equal(t, []string{"a/b/c/d", "a/b/c"}, resStr)
}
