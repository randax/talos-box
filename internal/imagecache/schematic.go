package imagecache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

var (
	requiredKernelArgs = []string{"console=tty0", "console=hvc0"}
	requiredExtensions = []string{"siderolabs/iscsi-tools", "siderolabs/util-linux-tools"}
)

// Schematic creates the content-addressed Image Factory schematic used by talosbox.
func (c *Cache) Schematic(extraArgs ...string) (string, error) {
	body, err := schematicRequestBody(nil, extraArgs)
	if err != nil {
		return "", err
	}
	return c.postSchematic(body)
}

// DefaultSchematic returns talosbox's own schematic id, composing it against
// the Factory only the first time. The id is recorded like any other
// composition so a restarted daemon — or an offline one — resolves the default
// combination from disk instead of posting again.
func (c *Cache) DefaultSchematic() (string, error) {
	id, found, err := c.CompositionID("", "", nil)
	if err != nil {
		return "", err
	}
	if found {
		return id, nil
	}
	id, err = c.Schematic()
	if err != nil {
		return "", err
	}
	if err := c.RecordDefaultSchematic(id); err != nil {
		return "", err
	}
	return id, nil
}

// RecordedDefaultSchematic returns the recorded default schematic id, if the
// default combination has ever been composed. Unlike DefaultSchematic it never
// contacts the Factory: retention decisions must not depend on the network.
func (c *Cache) RecordedDefaultSchematic() (string, bool, error) {
	return c.CompositionID("", "", nil)
}

// RecordDefaultSchematic remembers the default schematic id. The record is
// keyed on no base, no extensions, and no version: the default customization
// carries no version-dependent field, so one record answers every version.
func (c *Cache) RecordDefaultSchematic(id string) error {
	return c.RecordComposition("", "", nil, id)
}

func (c *Cache) postSchematic(body []byte) (string, error) {
	request, err := http.NewRequest(http.MethodPost, strings.TrimRight(c.factoryURL, "/")+"/schematics", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create schematic request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := c.schematicClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("post schematic: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return "", fmt.Errorf("post schematic: %s: %s", response.Status, strings.TrimSpace(string(message)))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode schematic response: %w", err)
	}
	if result.ID == "" {
		return "", errors.New("decode schematic response: missing id")
	}

	return result.ID, nil
}

type schematicRequest struct {
	Customization struct {
		ExtraKernelArgs  []string `json:"extraKernelArgs"`
		SystemExtensions struct {
			OfficialExtensions []string `json:"officialExtensions"`
		} `json:"systemExtensions"`
	} `json:"customization"`
}

// schematicRequestBody composes the talosbox customization: the required
// kernel arguments, the always-on storage extensions, and any additional
// official extension refs, deduplicated and sorted so the same request always
// content-addresses to the same schematic id.
func schematicRequestBody(extensionRefs, extraArgs []string) ([]byte, error) {
	request := schematicRequest{}
	request.Customization.ExtraKernelArgs = append(append([]string(nil), requiredKernelArgs...), extraArgs...)
	request.Customization.SystemExtensions.OfficialExtensions = mergeExtensions(extensionRefs)

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode schematic request: %w", err)
	}
	return body, nil
}

func mergeExtensions(extensionRefs []string) []string {
	merged := dedupeStrings(append(append([]string(nil), requiredExtensions...), extensionRefs...))
	sort.Strings(merged)
	return merged
}
