package mirror

import (
	"encoding/json"
	"fmt"
	"strings"
)

type manifestPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type manifestDescriptor struct {
	Digest string `json:"digest"`
}

type manifestIndexDescriptor struct {
	Digest   string           `json:"digest"`
	Platform manifestPlatform `json:"platform"`
}

type rawManifestGraph struct {
	SchemaVersion int                        `json:"schemaVersion"`
	MediaType     string                     `json:"mediaType"`
	Manifests     *[]manifestIndexDescriptor `json:"manifests"`
	Config        *manifestDescriptor        `json:"config"`
	Layers        *[]manifestDescriptor      `json:"layers"`
}

func decodeManifestGraph(data []byte) (cachedGraph, error) {
	var raw rawManifestGraph
	if err := json.Unmarshal(data, &raw); err != nil {
		return cachedGraph{}, fmt.Errorf("decode manifest graph: %w", err)
	}
	if err := validateManifestStructure(raw); err != nil {
		return cachedGraph{}, err
	}

	graph := cachedGraph{
		SchemaVersion: raw.SchemaVersion,
		MediaType:     raw.MediaType,
	}
	if raw.Manifests != nil {
		graph.Manifests = append(graph.Manifests, (*raw.Manifests)...)
	}
	if raw.Config != nil {
		graph.Config = *raw.Config
	}
	if raw.Layers != nil {
		graph.Layers = append(graph.Layers, (*raw.Layers)...)
	}
	return graph, nil
}

func validateManifestBytes(data []byte) error {
	var raw rawManifestGraph
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode manifest graph: %w", err)
	}
	return validateManifestStructure(raw)
}

func validateManifestStructure(graph rawManifestGraph) error {
	if graph.SchemaVersion != 2 {
		return fmt.Errorf("invalid manifest: schemaVersion %d, want 2", graph.SchemaVersion)
	}
	if manifestGraphIsIndex(graph) {
		if graph.Manifests == nil {
			return fmt.Errorf("invalid manifest: missing manifests array")
		}
		for i, descriptor := range *graph.Manifests {
			if err := validateManifestDigest(descriptor.Digest, fmt.Sprintf("manifest descriptor has invalid digest at manifests[%d]", i)); err != nil {
				return err
			}
		}
		return nil
	}
	if graph.Config == nil || graph.Config.Digest == "" {
		return fmt.Errorf("invalid manifest: missing config descriptor")
	}
	if err := validateManifestDigest(graph.Config.Digest, "config descriptor has invalid digest"); err != nil {
		return err
	}
	if graph.Layers == nil {
		return fmt.Errorf("invalid manifest: missing layers array")
	}
	for i, descriptor := range *graph.Layers {
		if err := validateManifestDigest(descriptor.Digest, fmt.Sprintf("layer descriptor has invalid digest at layers[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

func manifestGraphIsIndex(graph rawManifestGraph) bool {
	if strings.Contains(graph.MediaType, "index") || strings.Contains(graph.MediaType, "manifest.list") {
		return true
	}
	if strings.Contains(graph.MediaType, "image.manifest") || strings.Contains(graph.MediaType, "manifest.v2") {
		return false
	}
	return graph.Manifests != nil
}

func validateManifestDigest(reference, label string) error {
	if _, _, err := checkedSupportedDigest(reference); err != nil {
		return fmt.Errorf("invalid manifest: %s: %w", label, err)
	}
	return nil
}
