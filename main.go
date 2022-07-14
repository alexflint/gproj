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
	"google.golang.org/api/option"
	"google.golang.org/api/serviceusage/v1"
	"gopkg.in/yaml.v2"
)

const gprojFile = "googlecloudproject.yaml"

var ErrSpecNotFound = errors.New(gprojFile + " file not found")

func findProjectSpec() (string, error) {
	path, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for i := 0; i < 100; i++ {
		x := filepath.Join(path, gprojFile)
		if st, err := os.Stat(x); err == nil && st.Mode().IsRegular() {
			return x, nil
		}

		parent := filepath.Dir(path)
		if parent == path {
			return "", ErrSpecNotFound
		}

		path = parent
	}
	return "", errors.New("took more than 100 steps up parent hierarchy")
}

// ProjectSpec models the googlecloudproject.yaml file
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
			return nil, fmt.Errorf("error finding project specification: %w", err)
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

func available(ctx context.Context, args *args) error {
	// find the project spec
	spec, err := readProjectSpec(args.Spec)
	if err != nil {
		return err
	}

	resources, err := cloudresourcemanager.NewService(ctx,
		option.WithScopes(cloudresourcemanager.CloudPlatformScope))
	if err != nil {
		return err
	}

	// fetch the project, creating it if necessary
	project, err := resources.Projects.Get(spec.ID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("cannot list the available APIs before the project has been created... eep sorry")
	}

	apis, err := availableAPIs(ctx, project.ProjectNumber)
	if err != nil {
		return fmt.Errorf("error fetching available APIs: %w", err)
	}

	for _, api := range apis {
		if !strings.HasSuffix(api.Name, ".googleapis.com") && !args.Available.All {
			continue
		}
		if args.Available.Description {
			fmt.Printf("%-50s %s\n", api.Name, api.Summary)
		} else {
			fmt.Println(api.Name)
		}
	}

	return nil
}

func apply(ctx context.Context, args *args) error {
	// we do some hacky stuff to remove quota_project_id from the credentials json... ouch
	creds, err := googleCredentials(ctx)
	if err != nil {
		return err
	}

	// find the project spec
	spec, err := readProjectSpec(args.Spec)
	if err != nil {
		return err
	}

	if len(spec.Name) < 4 {
		return fmt.Errorf("project name %q invalid: must be at least 4 characters long (required by Google Cloud)", spec.Name)
	}

	resources, err := cloudresourcemanager.NewService(ctx,
		option.WithScopes(cloudresourcemanager.CloudPlatformScope),
		option.WithCredentials(creds))
	if err != nil {
		return err
	}

	// now enable the appropriate APIs
	apiService, err := serviceusage.NewService(ctx)
	if err != nil {
		return fmt.Errorf("error initializing the service usage API: %w", err)
	}

	// fetch the project, creating it if necessary
	project, err := resources.Projects.Get(spec.ID).Context(ctx).Do()
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
			project.Labels["managed-by"] = "gproj"

			// creating projects is a long-running operation so we have to poll
			createOp, err := resources.Projects.Create(project).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("error creating project: %w", err)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			err = waitForCreate(waitCtx, resources.Operations, createOp)
			if err != nil {
				return fmt.Errorf("error creating project: %w", err)
			}

			// now fetch the final project info containing the data filled in by the server
			project, err = resources.Projects.Get(spec.ID).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("error getting project info right after creating it: %w", err)
			}

			// enable the billing API, which we need in order to enable further APIs
			projNum := formatProjectNumber(project.ProjectNumber)
			enableOp, err := apiService.Services.BatchEnable(projNum, &serviceusage.BatchEnableServicesRequest{
				ServiceIds: []string{"cloudbilling.googleapis.com"},
			}).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("error in API call to enable APIs: %w", err)
			}

			err = waitForEnable(ctx, apiService.Operations, enableOp)
			if err != nil {
				return fmt.Errorf("error enabling billing API: %v", err)
			}

			fmt.Printf("created %s\n", spec.ID)
		} else {
			return err
		}
	}

	// initialize the billing service
	billing, err := cloudbilling.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return fmt.Errorf("error initializing the billing API: %w", err)
	}

	// get billing info for this account so that we know whether we need to change it
	projNum := formatProjectNumber(project.ProjectNumber)
	billingInfo, err := billing.Projects.GetBillingInfo(projNum).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("error getting billing info for %s: %w", spec.ID, err)
	}

	// find the requested billing account or look up the default
	account := spec.Billing
	if account == "enable" {
		fmt.Println("no billing account in spec, looking up available billing accounts...")
		accounts, err := billing.BillingAccounts.List().Context(ctx).Do()
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
	if billingInfo.BillingAccountName != account {
		fmt.Printf("updating billing account to %s\n", account)
		updatedBilling, err := billing.Projects.UpdateBillingInfo(projNum, &cloudbilling.ProjectBillingInfo{
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

	// now make a list of APIs to enable
	var toEnable []string
	for _, requestedAPI := range spec.APIs {
		if !strings.Contains(requestedAPI, ".") {
			repl := requestedAPI + ".googleapis.com"
			fmt.Printf("assuming that %q means %q\n", requestedAPI, repl)
			requestedAPI = repl
		}

		toEnable = append(toEnable, requestedAPI)
	}

	if len(toEnable) > 0 {
		fmt.Printf("enabling %d APIs:\n", len(toEnable))
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

		fmt.Println("this may take a minute or two...")
		err = waitForEnable(ctx, apiService.Operations, enableOp)
		if err != nil {
			return fmt.Errorf("error enabling %d APIs: %w", len(toEnable), err)
		}
	}

	// TODO: disable unused APIs

	return nil
}

func gcloud(ctx context.Context, args *args) error {
	// read the project spec
	spec, err := readProjectSpec(args.Spec)
	// if there is no .gcloud file so do nothing, or else "gcloud" will be inoperable
	if errors.Is(err, ErrSpecNotFound) {
		if args.Verbose {
			fmt.Println("no .gcloud file found, ignoring")
		}
	} else if err != nil {
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
	if !hasProjectArg && spec != nil {
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

func cmdDelete(ctx context.Context, args *args) error {
	// we do some hacky stuff to remove quota_project_id from the credentials json... ouch
	creds, err := googleCredentials(ctx)
	if err != nil {
		return err
	}

	// find the project spec
	spec, err := readProjectSpec(args.Spec)
	if err != nil {
		return err
	}

	resources, err := cloudresourcemanager.NewService(ctx,
		option.WithScopes(cloudresourcemanager.CloudPlatformScope),
		option.WithCredentials(creds))
	if err != nil {
		return err
	}

	_, err = resources.Projects.Delete(spec.ID).Context(ctx).Do()
	if err != nil {
		return err
	}

	fmt.Printf("Project %s has been deleted. To undelete in the next 30 days, run gproj undelete\n", spec.ID)
	return nil
}

// args for "gproj delete", which deletes the project
type deleteArgs struct {
}

// args for "gproj apply", which updates the project, the APIs, and the billing account
type applyArgs struct {
}

// args for "gproj available", which lists available APIs
type availableArgs struct {
	All         bool `help:"Include third-party services"`
	Description bool `help:"Print a line-line description of each API"`
}

// args for "gproj gcloud", which calls gcloud with a --project and --account added
type gcloudArgs struct {
	Args []string `arg:"positional"`
}

// args for the top-level gproj command
type args struct {
	Spec      string         `help:"path to config file"`
	Apply     *applyArgs     `arg:"subcommand"`
	Delete    *deleteArgs    `arg:"subcommand" help:"delete the current project"`
	Gcloud    *gcloudArgs    `arg:"subcommand"`
	Available *availableArgs `arg:"subcommand" help:"list available APIs"`
	Verbose   bool
}

func main() {
	ctx := context.Background()

	var args args
	p := arg.MustParse(&args)

	var err error
	switch {
	case args.Apply != nil:
		err = apply(ctx, &args)
	case args.Delete != nil:
		err = cmdDelete(ctx, &args)
	case args.Gcloud != nil:
		err = gcloud(ctx, &args)
	case args.Available != nil:
		err = available(ctx, &args)
	default:
		p.Fail("you must specify a command")
	}

	if err != nil {
		msg := err.Error()
		if !strings.HasPrefix(msg, "error") {
			msg = "error: " + msg
		}
		fmt.Println(msg)
		os.Exit(1)
	}
}
