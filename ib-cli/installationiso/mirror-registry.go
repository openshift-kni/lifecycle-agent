package installationiso

import (
	"fmt"

	"github.com/containers/image/pkg/sysregistriesv2"
	"github.com/pelletier/go-toml"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
)

func GenerateRegistryConf(imageDigestSources []ibiconfig.ImageDigestSource) (string, error) {
	registryConf, err := generateRegistriesConf(imageDigestSources)
	if err != nil {
		return "", fmt.Errorf("failed to generate registries.conf data: %w", err)
	}

	return string(registryConf), nil
}

func generateRegistriesConf(imageDigestSources []ibiconfig.ImageDigestSource) ([]byte, error) {
	registries := &sysregistriesv2.V2RegistriesConf{
		Registries: []sysregistriesv2.Registry{},
	}
	for _, group := range mergedMirrorSets(imageDigestSources) {
		if len(group.Mirrors) == 0 {
			continue
		}

		registry := sysregistriesv2.Registry{}
		registry.Endpoint.Location = group.Source
		registry.MirrorByDigestOnly = true
		for _, mirror := range group.Mirrors {
			registry.Mirrors = append(registry.Mirrors, sysregistriesv2.Endpoint{Location: mirror})
		}
		registries.Registries = append(registries.Registries, registry)
	}

	marshalled, err := toml.Marshal(registries)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal registries.conf data: %w", err)
	}

	return marshalled, nil
}

// MergedMirrorSets consolidates a list of ImageDigestSources so that each
// source appears only once.
func mergedMirrorSets(sources []ibiconfig.ImageDigestSource) []ibiconfig.ImageDigestSource {
	sourceSet := make(map[string][]string)
	mirrorSet := make(map[string]sets.Set[string])
	var orderedSources []string

	for _, group := range sources {
		if _, ok := sourceSet[group.Source]; !ok {
			orderedSources = append(orderedSources, group.Source)
			sourceSet[group.Source] = nil
			mirrorSet[group.Source] = sets.Set[string]{}
		}
		for _, mirror := range group.Mirrors {
			if !mirrorSet[group.Source].Has(mirror) {
				sourceSet[group.Source] = append(sourceSet[group.Source], mirror)
				mirrorSet[group.Source].Insert(mirror)
			}
		}
	}

	var out []ibiconfig.ImageDigestSource
	for _, source := range orderedSources {
		out = append(out, ibiconfig.ImageDigestSource{Source: source, Mirrors: sourceSet[source]})
	}
	return out
}
