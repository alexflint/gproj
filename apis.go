package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/api/serviceusage/v1"
)

// next make a map from API to enabled/disabled
type api struct {
	Name    string // machine readable name, e.g. "billingbudgets.googleapis.com"
	Title   string // human readable name, e.g. "Cloud Billing API"
	Summary string // one sentence description of the API
	Enabled bool
}

func formatProjectNumber(n int64) string {
	return fmt.Sprintf("projects/%d", n)
}

func cacheDir(projectNumber int64) (string, error) {
	cacheDir, err := os.UserCacheDir() // return ~/.cache on linux
	if err != nil {
		return "", fmt.Errorf("error getting user cache dir: %w", err)
	}

	path := filepath.Join(cacheDir, "gproj", strconv.FormatInt(projectNumber, 10))
	err = os.MkdirAll(path, os.ModePerm)
	if err != nil {
		return "", fmt.Errorf("error creating cache dir: %w", err)
	}

	return path, nil
}

// gets the first line of a string, or the whole string if it is only one line
func firstLine(s string) string {
	pos := strings.Index(s, "\n")
	if pos == -1 {
		return s
	}
	return s[:pos]
}

// get the available APIs from local cache or request from Google Cloud if missing
func availableAPIs(ctx context.Context, projectNumber int64) ([]*api, error) {
	cacheDir, err := cacheDir(projectNumber)
	if err != nil {
		// failed to create the cache dir so just do a pull and do not try to store the results
		fmt.Printf("warning: unable to cache results, error was: %v", err)
		return pullAvailableAPIs(ctx, projectNumber)
	}

	// try to look up the results from cache
	cachePath := filepath.Join(cacheDir, "available-apis.json")
	cached, err := os.ReadFile(cachePath)
	if err != nil {
		fmt.Println("fetching available APIs, this may take a minute or two...")
		return pullAndStoreAvailableAPIs(ctx, projectNumber, cachePath)
	}

	var apis []*api
	err = json.Unmarshal(cached, &apis)
	if err != nil {
		return nil, fmt.Errorf("error decoding cached API listing; try deleting %s: %v", cachePath, err)
	}

	return apis, nil
}

// pull the available APIs from Google Cloud and store to a file if successful
func pullAndStoreAvailableAPIs(ctx context.Context, projectNumber int64, path string) ([]*api, error) {
	apis, err := pullAvailableAPIs(ctx, projectNumber)
	if err != nil {
		return nil, err
	}

	buf, err := json.Marshal(apis)
	if err != nil {
		return nil, fmt.Errorf("error marshalling apis to json: %v", err)
	}

	err = os.WriteFile(path, buf, os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("error writing apis to cache: %v", err)
	}

	fmt.Printf("fetched %d APIs and stored at %s\n", len(apis), path)

	return apis, nil
}

// pull the available APIs from Google Cloud
func pullAvailableAPIs(ctx context.Context, projectNumber int64) ([]*api, error) {
	// create an API service to access the list of available APIs
	apiService, err := serviceusage.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("error initializing the service usage API: %w", err)
	}

	projNum := formatProjectNumber(projectNumber)

	// Fetch each page in turn
	var apis []*api
	err = apiService.Services.List(projNum).Pages(ctx, func(r *serviceusage.ListServicesResponse) error {
		for _, s := range r.Services {
			// note that s.Name is of the form "projects/123/services/foo.googleapis.com"
			// while s.Config.Name is of the form "foo.googleapis.com"
			api := api{
				Name:    s.Config.Name,
				Title:   s.Config.Title,
				Enabled: s.State == "ENABLED",
			}
			if s.Config.Documentation != nil {
				api.Summary = firstLine(s.Config.Documentation.Summary)
			}
			apis = append(apis, &api)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error getting list of APIs: %w", err)
	}

	sort.Slice(apis, func(i, j int) bool {
		return apis[i].Name < apis[j].Name
	})

	return apis, nil
}
