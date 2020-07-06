package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexflint/go-arg"
	"github.com/kr/pretty"
	"google.golang.org/api/cloudbilling/v1"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/serviceusage/v1"
	"gopkg.in/yaml.v2"
)

func findProjectSpec() (string, error) {
	path, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for i := 0; i < 100; i++ {
		x := filepath.Join(path, ".gproj")
		if st, err := os.Stat(x); err == nil && st.Mode().IsRegular() {
			return x, nil
		}

		parent := filepath.Dir(path)
		if parent == path {
			return "", errors.New(".gproj file not found")
		}

		path = parent
	}
	return "", errors.New("took more than 100 steps up parent hierarchy")
}

// ProjectSpec models the .gproj file
type ProjectSpec struct {
	Name    string            // human readable name of the project
	ID      string            // ID of the project (must also be input by hand)
	Labels  map[string]string // arbitrary key/value labels to assign to the project
	APIs    []string
	Billing string // ID of billing account, e.g. "012345-6789AB-CDEFG0" - leave empty to use default
}

func readProjectSpec(specPath string) (*ProjectSpec, error) {
	// if no spec path given on command line then work our way up from current dir
	if specPath == "" {
		var err error
		specPath, err = findProjectSpec()
		if err != nil {
			return nil, fmt.Errorf("error finding project spec: %w", err)
		}
	}

	// open the file
	f, err := os.Open(specPath)
	if err != nil {
		return nil, fmt.Errorf("error opening project spec: %w", err)
	}
	defer f.Close()

	// decode it
	var spec ProjectSpec
	err = yaml.NewDecoder(f).Decode(&spec)
	if err != nil {
		return nil, fmt.Errorf("error parsing project spec at %s: %w", specPath, err)
	}
	return &spec, nil
}

func waitForCreate(
	ctx context.Context,
	svc *cloudresourcemanager.OperationsService,
	op *cloudresourcemanager.Operation) error {

	name := op.Name

	if op.Error != nil {
		return fmt.Errorf("error performing operation: %v %v", op.Error.Code, op.Error.Message)
	}
	if op.Done {
		return nil
	}

	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		op, err := svc.Get(name).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("error getting operation info: %w", err)
		}
		if op.Error != nil {
			return fmt.Errorf("error performing operation: %v %v", op.Error.Code, op.Error.Message)
		}
		if op.Done {
			return nil
		}
	}

	return errors.New("ticker chanel was closed")
}

func waitForEnable(
	ctx context.Context,
	svc *serviceusage.OperationsService,
	op *serviceusage.Operation) error {

	name := op.Name

	if op.Error != nil {
		return fmt.Errorf("error performing operation: %v %v", op.Error.Code, op.Error.Message)
	}
	if op.Done {
		return nil
	}

	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		op, err := svc.Get(name).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("error getting operation info: %w", err)
		}
		if op.Error != nil {
			return fmt.Errorf("error performing operation: %v %v", op.Error.Code, op.Error.Message)
		}
		if op.Done {
			return nil
		}
	}

	return errors.New("ticker chanel was closed")
}

func sync(ctx context.Context, args *args) error {
	// find the project spec
	spec, err := readProjectSpec(args.Spec)
	if err != nil {
		return err
	}
	pretty.Println(spec)

	// TODO: validate the spec

	crmService, err := cloudresourcemanager.NewService(ctx)
	if err != nil {
		return err
	}

	// fetch the project, creating it if necessary
	project, err := crmService.Projects.Get(spec.ID).Context(ctx).Do()
	if err != nil {
		// we get "403 Forbidden" if the project does not exist since projects IDs
		// are global and Google doesn't want to reveal if the project exists
		// in someone else's account or not
		if e, ok := err.(*googleapi.Error); ok && e.Code == 403 {
			fmt.Printf("project %s does not exist, attempting to create it...\n", spec.ID)

			project = &cloudresourcemanager.Project{
				Name:      spec.Name,
				ProjectId: spec.ID,
				Labels:    make(map[string]string),
			}

			// deep copy the labels so that we can safely modify the map
			for k, v := range spec.Labels {
				project.Labels[k] = v
			}
			project.Labels["gproj-created-at"] = time.Now().String()

			// creating projects is a long-running operation so we have to poll
			createOp, err := crmService.Projects.Create(project).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("error creating project: %w", err)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			err = waitForCreate(waitCtx, crmService.Operations, createOp)
			if err != nil {
				return fmt.Errorf("error creating project: %w", err)
			}

			// now fetch the final project info containing the data filled in by the server
			project, err = crmService.Projects.Get(spec.ID).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("error getting project info right after creating it: %w", err)
			}

			fmt.Printf("created %s\n", spec.ID)
		} else {
			return err
		}
	}

	// for the service API and billing API, we use a project identifier of the form "projects/<number>"
	projNum := fmt.Sprintf("projects/%d", project.ProjectNumber)

	// initialize the billing service
	billingService, err := cloudbilling.NewService(ctx)
	if err != nil {
		return fmt.Errorf("error initializing the billing API: %w", err)
	}

	// get billing info for this account so that we know whether we need to change it
	billing, err := billingService.Projects.GetBillingInfo(projNum).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("error getting billing info for %s: %w", spec.ID, err)
	}

	// find the requested billing account or look up the default
	account := spec.Billing
	if account == "" {
		fmt.Println("no billing account in spec, looking up available billing accounts...")
		accounts, err := billingService.BillingAccounts.List().Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("error listing billing accounts: %w", err)
		}

		// make a list of open accounts
		var openAccounts []*cloudbilling.BillingAccount
		for _, a := range accounts.BillingAccounts {
			if a.Open {
				openAccounts = append(openAccounts, a)
			}
		}

		if len(openAccounts) == 1 {
			fmt.Printf("using the only open billing account %q (%s)\n", openAccounts[0].Name, openAccounts[0].DisplayName)
			account = openAccounts[0].Name
		} else {
			return fmt.Errorf(
				"no billing account in spec and found %d billing accounts (of which %d were open)",
				len(accounts.BillingAccounts),
				len(openAccounts))
		}
	}

	// update the billing account
	if billing.BillingAccountName != account {
		fmt.Printf("updating billing account to %s\n", account)
		updatedBilling, err := billingService.Projects.UpdateBillingInfo(projNum, &cloudbilling.ProjectBillingInfo{
			BillingAccountName: account,
		}).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("error updating billing info: %w", err)
		}

		// check that billing is now enabled
		if !updatedBilling.BillingEnabled {
			return fmt.Errorf(
				"billing account was updated but API response shows billing still not enabled:\n%s",
				pretty.Sprint(updatedBilling))
		}

		fmt.Println("updated billing info")
	}

	// now enable the appropriate APIs
	apiService, err := serviceusage.NewService(ctx)
	if err != nil {
		return fmt.Errorf("error initializing the service usage API: %w", err)
	}

	apiList, err := apiService.Services.List(projNum).Do()
	if err != nil {
		return fmt.Errorf("error getting list of enabled services for %s: %w", project.ProjectId, err)
	}

	// next make a map from API to enabled/disabled
	type api struct {
		name    string // machine readable name, e.g. "billingbudgets.googleapis.com"
		title   string // human readable name, e.g. "Cloud Billing API"
		summary string // one sentence description of the API
		enabled bool
		service *serviceusage.GoogleApiServiceusageV1ServiceConfig // note that most of these fields seem to be empty for most APIs
	}

	apis := make(map[string]*api)
	for _, s := range apiList.Services {
		// note that s.Name is of the form "projects/123/services/foo.googleapis.com"
		// while s.Config.Name is of the form "foo.googleapis.com"
		apis[s.Config.Name] = &api{
			name:    s.Config.Name,
			title:   s.Config.Title,
			enabled: s.State == "ENABLED",
			service: nil, //s.Config,
		}
		if s.Config.Documentation != nil {
			apis[s.Config.Name].summary = s.Config.Documentation.Summary
		}
	}

	// now make a list of APIs to enable
	var toEnable []string
	for _, requestedAPI := range spec.APIs {
		if !strings.Contains(requestedAPI, ".") {
			repl := requestedAPI + ".googleapis.com"
			fmt.Printf("assuming that %q means %q\n", requestedAPI, repl)
			requestedAPI = repl
		}

		info, ok := apis[requestedAPI]
		if !ok {
			return fmt.Errorf("no such API: %s", requestedAPI)
		}
		if !info.enabled {
			toEnable = append(toEnable, requestedAPI)
		}
	}

	if len(toEnable) > 0 {
		fmt.Printf("Enabling %d APIs:\n", len(toEnable))
		for _, api := range toEnable {
			fmt.Printf("  %s\n", api)
		}

		if len(toEnable) > 20 {
			return fmt.Errorf("cannot enable more than 20 APIs at a time")
		}

		// do a batch update
		enableOp, err := apiService.Services.BatchEnable(projNum, &serviceusage.BatchEnableServicesRequest{
			ServiceIds: toEnable,
		}).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("error in API call to enable APIs: %w", err)
		}

		err = waitForEnable(ctx, apiService.Operations, enableOp)
		if err != nil {
			return fmt.Errorf("error enabling %d APIs: %w", len(toEnable), err)
		}
	}

	return nil
}

func gcloud(ctx context.Context, args *args) error {
	// read the project spec
	spec, err := readProjectSpec(args.Spec)
	if err != nil {
		return err
	}

	// add --project=PROJECT unless there is already a --project argument manually provided
	var hasProjectArg bool
	for _, arg := range args.Gcloud.Args {
		if strings.HasPrefix(arg, "--project") {
			hasProjectArg = true
			break
		}
	}

	gcloudArgs := args.Gcloud.Args
	if !hasProjectArg {
		gcloudArgs = append([]string{"--project=" + spec.ID}, args.Gcloud.Args...)
	}

	// run the subcommand
	gcloudPath, err := exec.LookPath("gcloud")
	if err != nil {
		return err
	}

	subcmd := exec.Command(gcloudPath, gcloudArgs...)
	subcmd.Stdin = os.Stdin
	subcmd.Stdout = os.Stdout
	subcmd.Stderr = os.Stderr
	err = subcmd.Run()

	var exitCode int
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			// this represents a non-zero exit code, so just pass it along
			exitCode = e.ExitCode()
		} else {
			return fmt.Errorf("error executing 'gcloud %s': %w", strings.Join(gcloudArgs, " "), err)
		}
	}

	// exit with withever exit code the subcommand gave
	os.Exit(exitCode)

	// this is unreachable but we must return or else lint will complain
	return nil
}

type syncArgs struct {
}

type gcloudArgs struct {
	Args []string `arg:"positional"`
}

type args struct {
	Spec   string      `help:"path to config file"`
	Sync   *syncArgs   `arg:"subcommand"`
	Gcloud *gcloudArgs `arg:"subcommand"`
}

func main() {
	ctx := context.Background()

	var args args
	p := arg.MustParse(&args)

	var err error

	switch {
	case args.Sync != nil:
		err = sync(ctx, &args)
	case args.Gcloud != nil:
		err = gcloud(ctx, &args)
	default:
		p.Fail("you must specify a command")
	}

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
