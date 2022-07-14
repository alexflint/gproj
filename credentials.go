package main

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"

	"golang.org/x/oauth2/google"
)

// regular expressions for removing "quota_project_id": "*" from credentials (hacky!!)
var quotaProjectPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\,\s*"quota_project_id":\s*"[^"]*"`),      // comma before
	regexp.MustCompile(`"quota_project_id":\s*"[^"]*"\s*\,`),      // comma after
	regexp.MustCompile(`\{\s*"quota_project_id":\s*"[^"]*"\s*\}`), // braces before and after
}

func PrettyJSON(b []byte) string {
	var buf bytes.Buffer
	json.Indent(&buf, b, "", "  ")
	return buf.String()
}

// It seems that we need to have a project with cloud resource manager API already
// created in order to do this. But "gcloud projects create" seems to work even
// without a "master" project to work from

// here is how gcloud projects create works:
//   https://github.com/twistedpair/google-cloud-sdk/blob/master/google-cloud-sdk/lib/googlecloudsdk/surface/projects/create.py#L40
// and here is how the resource manager client is created:
//   https://github.com/twistedpair/google-cloud-sdk/blob/master/google-cloud-sdk/lib/googlecloudsdk/surface/projects/__init__.py#L22
// it might be significant that get_credentials is set to False in the above
// that parameter is processed by the google apitools logic here:
//   https://github.com/google/apitools/blob/master/apitools/base/py/base_api.py#L240
// in the end it triggers a call to _GetCredentials if it is set to true:
//   https://github.com/google/apitools/blob/31cad2d904f356872d2965687e84b2d87ee2cdd3/apitools/base/py/base_api.py#L312
// perhaps on the golang side we could try option.WithoutAuthentication but then how does authentication
// ultimately get in there?
//   https://pkg.go.dev/google.golang.org/api/option#WithoutAuthentication

// SOLVED! The solution is to remove the key "quota_project_id" from application_default_credentials.json
// eep what a mess...
func googleCredentials(ctx context.Context, scopes ...string) (*google.Credentials, error) {
	// first get the credentials so that we use the google logic for where the credentials should come from
	creds, err := google.FindDefaultCredentials(ctx, scopes...)
	if err != nil {
		return nil, err
	}

	// Annoyingly, cloud resource manager fails if the application default credentials
	// specify a project that does not explicitly have the cloud resource manager API
	// enabled. But we are using cloud resource manager to *create* our project so we
	// can hardly expect to already have a project with the appropriate APIs enabled.
	// Cloud resource manager actually succeeds if *no* project is specified, but
	// unfortunately we need to mess with the credentials object in order to do that:
	creds.ProjectID = ""
	creds.JSON = nil
	return creds, nil
}
