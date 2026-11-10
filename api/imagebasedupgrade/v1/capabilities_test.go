/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	"strings"
	"testing"
)

// TestSeedFormatContractConstants locks the exact values of the exported seed
// format capability contract. A hub consumer (TALM/IBGU) depends on these by
// import, so an accidental change here would silently break the contract.
func TestSeedFormatContractConstants(t *testing.T) {
	// SeedFormatVersion stays 4 in this release: PR#7818 owns the bump to 5, so
	// relocating the constant here must not change its value.
	if SeedFormatVersion != 4 {
		t.Errorf("SeedFormatVersion = %d, want %d", SeedFormatVersion, 4)
	}

	// BackupContentFirstSeedFormatVersion is 5, not 4: backupContent ships in the
	// same backport unit as the format bump, so 5 is the first format that
	// supports it. It is intentionally NOT equal to (nor <=) SeedFormatVersion in
	// this release; the equality against SeedFormatVersion is added by PR#7818 when
	// it bumps SeedFormatVersion to 5.
	if BackupContentFirstSeedFormatVersion != 5 {
		t.Errorf("BackupContentFirstSeedFormatVersion = %d, want %d", BackupContentFirstSeedFormatVersion, 5)
	}

	// The OCI label key is stamped onto seed images already in the field, so it
	// must never change.
	if SeedFormatOCILabel != "com.openshift.lifecycle-agent.seed_format_version" {
		t.Errorf("SeedFormatOCILabel = %q, want %q",
			SeedFormatOCILabel, "com.openshift.lifecycle-agent.seed_format_version")
	}

	if SeedFormatClusterClaimName == "" {
		t.Error("SeedFormatClusterClaimName must not be empty")
	}
	// The ".openshift.io" suffix is a hard requirement: it is what makes the claim
	// exempt from OCM's per-spoke custom-claim cap on ACM 2.14+.
	if !strings.HasSuffix(SeedFormatClusterClaimName, ".openshift.io") {
		t.Errorf("SeedFormatClusterClaimName = %q, want a name ending in %q",
			SeedFormatClusterClaimName, ".openshift.io")
	}
	if SeedFormatClusterClaimName != "seedformatversion.lca.openshift.io" {
		t.Errorf("SeedFormatClusterClaimName = %q, want %q",
			SeedFormatClusterClaimName, "seedformatversion.lca.openshift.io")
	}

	if SeedFormatVersionStatusField == "" {
		t.Error("SeedFormatVersionStatusField must not be empty")
	}
	if SeedFormatVersionStatusField != "seedFormatVersion" {
		t.Errorf("SeedFormatVersionStatusField = %q, want %q",
			SeedFormatVersionStatusField, "seedFormatVersion")
	}
}
