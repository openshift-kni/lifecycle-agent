package prep

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetDeploymentFromDeploymentID(t *testing.T) {
	testcases := []struct {
		name         string
		deploymentID string
		expect       string
		err          error
	}{
		{
			name:         "normal version",
			deploymentID: "rhcos-ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			expect:       "ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			err:          nil,
		},
		{
			name:         "multiple dashes",
			deploymentID: "4.15.0-0.nightly-2023-12-04-223539-ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			expect:       "ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			err:          nil,
		},
		{
			name:         "no dashes",
			deploymentID: "ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1",
			expect:       "",
			err: fmt.Errorf(
				"failed to get deployment from deploymentID, there should be a '-' in deployment"),
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := getDeploymentFromDeploymentID(tc.deploymentID)
			assert.Equal(t, tc.expect, res)
			if tc.err != nil {
				assert.ErrorContains(t, err, tc.err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
