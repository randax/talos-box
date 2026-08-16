package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/daemon"
	"github.com/randax/talos-box/internal/extensions"
	"github.com/randax/talos-box/internal/talosversion"
)

// runCachePull makes disk-image combinations available offline. With no flags
// it reads talosbox.yaml the way `tbx up` does and pulls every distinct
// combination the file declares — re-compositions included, which is why it
// has to run while the Factory is still reachable. Explicit flags pull one
// ad-hoc combination instead. Either way, what is pulled is pinned.
func (c cli) runCachePull(args []string) error {
	flags := flag.NewFlagSet("cache pull", flag.ContinueOnError)
	flags.SetOutput(c.err)
	talosVersion := flags.String("talos-version", daemon.DefaultTalosVersion, "Talos version")
	schematic := flags.String("schematic", "", "Image Factory schematic")
	extensionList := flags.String("extensions", "", "curated Talos extensions, comma-separated: "+strings.Join(extensions.Names(), "|"))
	path := flags.String("f", defaultConfigFile, "path to talosbox.yaml")
	noImages := flags.Bool("no-images", false, "pull disk images only, without warming the clusters' container images")
	positionals, err := parseInterspersed(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		return errors.New("usage: tbx cache pull [-f talosbox.yaml] [--no-images] [--talos-version VERSION --schematic ID --extensions LIST]")
	}
	provided := map[string]bool{}
	flags.Visit(func(item *flag.Flag) { provided[item.Name] = true })

	var combinations []daemon.CachePullCombination
	fromFile := !provided["talos-version"] && !provided["schematic"] && !provided["extensions"]
	if !fromFile {
		combinations = []daemon.CachePullCombination{{
			Schematic: *schematic, Version: *talosVersion, Extensions: parseExtensionList(*extensionList),
		}}
	} else if combinations, err = configPullCombinations(*path, provided["f"], *talosVersion); err != nil {
		return err
	}
	// Both checks are local: a typo or an out-of-window version must be
	// reported here, before the daemon starts or the Factory is contacted.
	requiresExtensions := false
	for _, combination := range combinations {
		if combination.Version != "" {
			if err := talosversion.Validate(combination.Version); err != nil {
				return err
			}
		}
		if _, err := extensions.Resolve(combination.Extensions); err != nil {
			return err
		}
		requiresExtensions = requiresExtensions || len(combination.Extensions) > 0
	}
	// Combinations, FromFile, and SkipImages only exist at protocol 5. Only
	// the single extension-free ad-hoc pull round-trips safely through the
	// scalar fields below; every fromFile form ships FromFile, which alone
	// turns on warming, pinning, and stray reporting, so an older daemon
	// dropping it would silently break the offline promise. Refuse instead.
	if requiresExtensions || fromFile || *noImages {
		if err := c.ensurePerClusterTalosSupport(); err != nil {
			return err
		}
	}

	request := daemon.CachePullArgs{Combinations: combinations, FromFile: fromFile, SkipImages: *noImages}
	if len(combinations) == 1 {
		// A daemon predating the combination list reads only these, and
		// then pulls exactly the one combination that was asked for.
		request.Schematic, request.Version = combinations[0].Schematic, combinations[0].Version
	}
	var result daemon.CachePullResult
	if err := c.call("cache.pull", request, &result); err != nil {
		return err
	}
	images := result.Images
	if len(images) == 0 {
		images = []daemon.CachePullImage{{
			Schematic: result.Schematic, Version: result.Version,
			Architecture: result.Architecture, Path: result.Path,
		}}
	}
	for _, image := range images {
		if _, err := fmt.Fprintf(c.out, "cached Talos %s %s schematic %s at %s\n",
			image.Version, image.Architecture, image.Schematic, image.Path); err != nil {
			return err
		}
	}
	if err := printWarmedImages(c.out, result.Warm); err != nil {
		return err
	}
	if err := printStrayImages(c.out, result.Strays); err != nil {
		return err
	}
	if result.Warm != nil && result.Warm.Failed > 0 {
		return fmt.Errorf("cache warm failed for %d ref(s)", result.Warm.Failed)
	}
	return nil
}

// configPullCombinations reads the desired combinations from talosbox.yaml.
// A missing default file is not an error: `tbx cache pull` outside a project
// still means "the default combination".
func configPullCombinations(path string, explicit bool, defaultVersion string) ([]daemon.CachePullCombination, error) {
	fallback := []daemon.CachePullCombination{{Version: defaultVersion}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return fallback, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, err
	}
	var combinations []daemon.CachePullCombination
	seen := map[string]struct{}{}
	for _, spec := range cfg.Clusters {
		// spec.Talos already carries the file-level block with the
		// cluster's field-wise overrides applied.
		combination := daemon.CachePullCombination{
			Schematic: spec.Talos.Schematic, Version: spec.Talos.Version, Extensions: spec.Talos.Extensions,
			Intent: spec.ProvisioningIntent,
		}
		// Two clusters collapse into one combination only when they also
		// install the same thing: the intent decides which images get warmed.
		key := strings.Join(append([]string{
			combination.Schematic, combination.Version, fmt.Sprintf("%+v", combination.Intent),
		}, combination.Extensions...), "\x00")
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		combinations = append(combinations, combination)
	}
	if len(combinations) == 0 {
		return fallback, nil
	}
	return combinations, nil
}
