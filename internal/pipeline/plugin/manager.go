// Copyright 2022 Harness Inc. All rights reserved.
// Use of this source code is governed by the Polyform Free Trial License
// that can be found in the LICENSE.md file for this repository.

package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/harness/gitness/internal/store"
	"github.com/harness/gitness/types"

	v1yaml "github.com/drone/spec/dist/go"
	"github.com/drone/spec/dist/go/parse"
	"github.com/rs/zerolog/log"
)

// Lookup returns a resource by name, kind and type.
type LookupFunc func(name, kind, typ, version string) (*v1yaml.Config, error)

type PluginManager struct {
	config      *types.Config
	pluginStore store.PluginStore
}

func NewPluginManager(
	config *types.Config,
	pluginStore store.PluginStore,
) *PluginManager {
	return &PluginManager{
		config:      config,
		pluginStore: pluginStore,
	}
}

// GetLookupFn returns a lookup function for plugins which can be used in the resolver.
func (m *PluginManager) GetLookupFn() LookupFunc {
	return func(name, kind, typ, version string) (*v1yaml.Config, error) {
		if kind != "plugin" {
			return nil, fmt.Errorf("only plugin kind supported")
		}
		if typ != "step" {
			return nil, fmt.Errorf("only step plugins supported")
		}
		plugin, err := m.pluginStore.Find(context.Background(), name, version)
		if err != nil {
			return nil, fmt.Errorf("could not lookup plugin: %w", err)
		}
		// Convert plugin to v1yaml spec
		config, err := parse.ParseString(plugin.Spec)
		if err != nil {
			return nil, fmt.Errorf("could not unmarshal plugin to v1yaml spec: %w", err)
		}

		return config, nil
	}
}

// Populate fetches plugins information from an external source or a local zip
// and populates in the DB.
func (m *PluginManager) Populate(ctx context.Context) error {
	path := m.config.CI.PluginsZipPath
	if path == "" {
		return fmt.Errorf("plugins path not provided to read schemas from")
	}

	var zipFile *zip.ReadCloser
	if _, err := os.Stat(path); err != nil { // local path doesn't exist - must be a remote link
		// Download zip file locally
		f, err := os.CreateTemp(os.TempDir(), "plugins.zip")
		if err != nil {
			return fmt.Errorf("could not create temp file: %w", err)
		}
		defer os.Remove(f.Name())
		err = downloadZip(path, f.Name())
		if err != nil {
			return fmt.Errorf("could not download remote zip: %w", err)
		}
		path = f.Name()
	}
	// open up a zip reader for the file
	zipFile, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("could not open zip for reading: %w", err)
	}
	defer zipFile.Close()

	// upsert any new plugins.
	err = m.traverseAndUpsertPlugins(ctx, zipFile)
	if err != nil {
		return fmt.Errorf("could not upsert plugins: %w", err)
	}

	return nil
}

// downloadZip is a helper function that downloads a zip from a URL and
// writes it to a path in the local filesystem.
func downloadZip(url, path string) error {
	response, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("could not get zip from url: %w", err)
	}
	defer response.Body.Close()

	// Create the file on the local FS. If it exists, it will be truncated.
	output, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("could not create output file: %w", err)
	}
	defer output.Close()

	// Copy the zip output to the file.
	_, err = io.Copy(output, response.Body)
	if err != nil {
		return fmt.Errorf("could not copy response body output to file: %w", err)
	}

	return nil
}

// traverseAndUpsertPlugins traverses through the zip and upserts plugins into the database
// if they are not present.
func (m *PluginManager) traverseAndUpsertPlugins(ctx context.Context, rc *zip.ReadCloser) error {
	plugins, err := m.pluginStore.ListAll(ctx)
	if err != nil {
		return fmt.Errorf("could not list plugins: %w", err)
	}
	// Put the plugins in a map so we don't have to perform frequent DB queries.
	pluginMap := map[string]*types.Plugin{}
	for _, p := range plugins {
		pluginMap[p.UID] = p
	}
	cnt := 0
	for _, file := range rc.File {
		matched, err := filepath.Match("**/plugins/*/*.yaml", file.Name)
		if err != nil { // only returns BadPattern error which shouldn't happen
			return fmt.Errorf("could not glob pattern: %w", err)
		}
		if !matched {
			continue
		}
		fc, err := file.Open()
		if err != nil {
			log.Warn().Err(err).Str("name", file.Name).Msg("could not open file")
			continue
		}
		defer fc.Close()
		var buf bytes.Buffer
		_, err = io.Copy(&buf, fc)
		if err != nil {
			log.Warn().Err(err).Str("name", file.Name).Msg("could not read file contents")
			continue
		}
		// schema should be a valid config - if not log an error and continue.
		config, err := parse.ParseBytes(buf.Bytes())
		if err != nil {
			log.Warn().Err(err).Str("name", file.Name).Msg("could not parse schema into valid config")
			continue
		}

		var desc string
		switch vv := config.Spec.(type) {
		case *v1yaml.PluginStep:
			desc = vv.Description
		case *v1yaml.PluginStage:
			desc = vv.Description
		default:
			log.Warn().Str("name", file.Name).Msg("schema did not match a valid plugin schema")
			continue
		}

		plugin := &types.Plugin{
			Description: desc,
			UID:         config.Name,
			Type:        config.Type,
			Spec:        buf.String(),
		}

		// Try to read the logo if it exists in the same directory
		dir := filepath.Dir(file.Name)
		logoFile := filepath.Join(dir, "logo.svg")
		if lf, err := rc.Open(logoFile); err == nil { // if we can open the logo file
			var lbuf bytes.Buffer
			_, err = io.Copy(&lbuf, lf)
			if err != nil {
				log.Warn().Err(err).Str("name", file.Name).Msg("could not copy logo file")
			} else {
				plugin.Logo = lbuf.String()
			}
		}

		// If plugin already exists in the database, skip upsert
		if p, ok := pluginMap[plugin.UID]; ok {
			if p.Matches(plugin) {
				continue
			}

		}

		// If plugin name exists with a different spec, call update - otherwise call create.
		// TODO: Once we start using versions, we can think of whether we want to
		// keep different schemas for each version in the database. For now, we will
		// simply overwrite the existing version with the new version.
		if _, ok := pluginMap[plugin.UID]; ok {
			err = m.pluginStore.Update(ctx, plugin)
			if err != nil {
				log.Warn().Str("name", file.Name).Err(err).Msg("could not update plugin")
				continue
			}
			log.Info().Str("name", file.Name).Msg("detected changes: updated existing plugin entry")
		} else {
			err = m.pluginStore.Create(ctx, plugin)
			if err != nil {
				log.Warn().Str("name", file.Name).Err(err).Msg("could not create plugin in DB")
				continue
			}
			cnt++
		}
	}
	log.Info().Msgf("added %d new entries to plugins", cnt)
	return nil
}