package imagecache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var requiredKernelArgs = []string{"console=tty0", "console=hvc0"}

// Schematic creates the content-addressed Image Factory schematic used by talosbox.
func (c *Cache) Schematic(extraArgs ...string) (string, error) {
	body, err := schematicRequestBody(extraArgs)
	if err != nil {
		return "", err
	}

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
		ExtraKernelArgs []string `json:"extraKernelArgs"`
	} `json:"customization"`
}

func schematicRequestBody(extraArgs []string) ([]byte, error) {
	request := schematicRequest{}
	request.Customization.ExtraKernelArgs = append(append([]string(nil), requiredKernelArgs...), extraArgs...)

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode schematic request: %w", err)
	}
	return body, nil
}
