package support

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/openshift-online/ocm-cli/pkg/arguments"
	"github.com/openshift-online/ocm-cli/pkg/dump"
	sdk "github.com/openshift-online/ocm-sdk-go"
	v1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	"github.com/openshift/osdctl/internal/support"
	"github.com/openshift/osdctl/internal/utils"
	"github.com/openshift/osdctl/internal/utils/globalflags"
	ctlutil "github.com/openshift/osdctl/pkg/utils"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

var (
	template                                                string
	templateParams, userParameterNames, userParameterValues []string
)

const (
	defaultTemplate = ""
)

type postOptions struct {
	output         string
	verbose        bool
	clusterID      string
	isDryRun       bool
	limitedSupport support.LimitedSupport

	genericclioptions.IOStreams
	GlobalOptions *globalflags.GlobalOptions
}

func newCmdpost(streams genericclioptions.IOStreams, globalOpts *globalflags.GlobalOptions) *cobra.Command {

	ops := newPostOptions(streams, globalOpts)
	postCmd := &cobra.Command{
		Use:               "post CLUSTER_ID",
		Short:             "Send limited support reason to a given cluster",
		Args:              cobra.ExactArgs(1),
		DisableAutoGenTag: true,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(ops.complete(cmd, args))
			cmdutil.CheckErr(ops.run())
		},
	}

	// Define required flags
	postCmd.Flags().StringVarP(&template, "template", "t", defaultTemplate, "Message template file or URL")
	postCmd.Flags().BoolVarP(&ops.isDryRun, "dry-run", "d", false, "Dry-run - print the limited support reason about to be sent but don't send it.")
	postCmd.Flags().StringArrayVarP(&templateParams, "param", "p", templateParams, "Specify a key-value pair (eg. -p FOO=BAR) to set/override a parameter value in the template.")
	postCmd.Flags().BoolVarP(&ops.verbose, "verbose", "", false, "Verbose output")

	return postCmd
}

func newPostOptions(streams genericclioptions.IOStreams, globalOpts *globalflags.GlobalOptions) *postOptions {

	return &postOptions{
		IOStreams:     streams,
		GlobalOptions: globalOpts,
	}
}

func (o *postOptions) complete(cmd *cobra.Command, args []string) error {

	if len(args) != 1 {
		return cmdutil.UsageErrorf(cmd, "Provide exactly one internal cluster ID")
	}

	o.clusterID = args[0]
	o.output = o.GlobalOptions.Output

	return nil
}

func (o *postOptions) run() error {

	// Parse the given JSON template provided via '-t' flag
	// and load it into the limitedSupport variable
	o.readTemplate()

	// Parse all the '-p' user flags
	parseUserParameters()

	// Check that the cluster key (name, identifier or external identifier) given by the user
	// is reasonably safe so that there is no risk of SQL injection
	err := ctlutil.IsValidClusterKey(o.clusterID)
	if err != nil {
		return err
	}

	// For every '-p' flag, replace it's related placeholder in the template
	for k := range userParameterNames {
		o.replaceWithFlags(userParameterNames[k], userParameterValues[k])
	}

	//if the cluster key is on the right format
	//create connection to sdk
	connection, err := ctlutil.CreateConnection()
	if err != nil {
		return err
	}
	defer func() {
		if err := connection.Close(); err != nil {
			fmt.Printf("Cannot close the connection: %q\n", err)
			os.Exit(1)
		}
	}()

	// Print limited support template to be sent
	fmt.Printf("The following limited support reason will be sent to %s:\n", o.clusterID)
	if err := o.printTemplate(); err != nil {
		fmt.Printf("Cannot read generated template: %q\n", err)
		os.Exit(1)
	}

	// Stop here if dry-run
	if o.isDryRun {
		return nil
	}

	// ConfirmPrompt prompt to confirm
	if !ctlutil.ConfirmPrompt() {
		return nil
	}

	//getting the cluster
	cluster, err := ctlutil.GetCluster(connection, o.clusterID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Can't retrieve cluster: %v\n", err)
		os.Exit(1)
	}

	// postRequest calls createPostRequest and take in client and clustersmgmt/v1.cluster object
	postRequest, err := o.createPostRequest(connection, cluster)
	if err != nil {
		fmt.Printf("failed to create post request %q\n", err)
	}
	postResponse, err := sendRequest(postRequest)
	if err != nil {
		fmt.Printf("Failed to get post call response: %q\n", err)
	}

	// check if response matches limitedSupport
	err = check(postResponse)
	if err != nil {
		fmt.Printf("Failed to check postResponse %q\n", err)
	}
	return nil
}

// createPostRequest create and populates the limited support post call
// swagger code gen: https://api.openshift.com/?urls.primaryName=Clusters%20management%20service#/default/post_api_clusters_mgmt_v1_clusters__cluster_id__limited_support_reasons
// SDKConnection is an interface that is satisfied by the sdk.Connection and by our mock connection
// this facilitates unit test and allow us to mock Post() and Delete() api calls
func (o *postOptions) createPostRequest(ocmClient SDKConnection, cluster *v1.Cluster) (request *sdk.Request, err error) {

	targetAPIPath := "/api/clusters_mgmt/v1/clusters/" + cluster.ID() + "/limited_support_reasons"

	request = ocmClient.Post()
	err = arguments.ApplyPathArg(request, targetAPIPath)
	if err != nil {
		return nil, fmt.Errorf("cannot parse API path '%s': %v", targetAPIPath, err)
	}

	messageBytes, err := json.Marshal(o.limitedSupport)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal template to json: %v", err)
	}

	request.Bytes(messageBytes)
	return request, nil
}

// readTemplate loads the template into the limitedSupport variable
func (o *postOptions) readTemplate() {

	if template == defaultTemplate {
		log.Fatalf("Template file is not provided. Use '-t' to fix this.")
	}

	// check if this URL or file and if we can access it
	file, err := accessFile(template)
	if err != nil {
		log.Fatal(err)
	}

	if err = o.parseTemplate(file); err != nil {
		log.Fatalf("Cannot not parse the JSON template.\nError: %q\n", err)
	}
}

// accessTemplate returns the contents of a local file or url, and any errors encountered
func accessFile(filePath string) ([]byte, error) {

	// when template is file on disk
	if utils.FileExists(filePath) {
		file, err := os.ReadFile(filePath) //#nosec G304 -- filePath cannot be constant
		if err != nil {
			return file, fmt.Errorf("cannot read the file.\nError: %q", err)
		}
		return file, nil
	}
	if utils.FolderExists(filePath) {
		return nil, fmt.Errorf("the provided path %q is a directory, not a file", filePath)
	}

	// when template is URL
	if utils.IsValidUrl(filePath) {
		urlPage, _ := url.Parse(filePath)
		if err := utils.IsOnline(*urlPage); err != nil {
			return nil, fmt.Errorf("host %q is not accessible", filePath)
		}
		return utils.CurlThis(urlPage.String())
	}
	return nil, fmt.Errorf("cannot read the file %q", filePath)
}

// parseTemplate reads the template file into a JSON struct
func (o *postOptions) parseTemplate(jsonFile []byte) error {
	return json.Unmarshal(jsonFile, &o.limitedSupport)
}

func (o *postOptions) printTemplate() error {

	limitedSupportMessage, err := json.Marshal(o.limitedSupport)
	if err != nil {
		return err
	}
	return dump.Pretty(os.Stdout, limitedSupportMessage)
}

func validateGoodResponse(body []byte) (goodReply *support.GoodReply, err error) {

	if !json.Valid(body) {
		return nil, fmt.Errorf("Server returned invalid JSON")
	}

	if err = json.Unmarshal(body, &goodReply); err != nil {
		return nil, fmt.Errorf("Cannot parse JSON template.\nError: %q", err)
	}
	return goodReply, nil
}

func validateBadResponse(body []byte) (badReply *support.BadReply, err error) {

	if ok := json.Valid(body); !ok {
		return nil, fmt.Errorf("Server returned invalid JSON")
	}
	if err = json.Unmarshal(body, &badReply); err != nil {
		return nil, fmt.Errorf("Cannot parse the error JSON meessage: %q", err)
	}
	return badReply, nil
}

func check(response *sdk.Response) error {

	body := response.Bytes()
	if response.Status() == http.StatusCreated {
		_, err := validateGoodResponse(body)
		if err != nil {
			return fmt.Errorf("failed to validate good response: %q", err)
		}
		fmt.Printf("Limited support reason has been sent successfully\n")
		return nil
	}

	badReply, err := validateBadResponse(body)
	if err != nil {
		return fmt.Errorf("failed to validate bad response: %v", err)
	}
	return fmt.Errorf("bad response reason is: %s", badReply.Reason)
}

// parseUserParameters parse all the '-p FOO=BAR' parameters and checks for syntax errors
func parseUserParameters() {
	for _, v := range templateParams {
		if !strings.Contains(v, "=") {
			log.Fatalf("Wrong syntax of '-p' flag. Please use it like this: '-p FOO=BAR'")
		}

		param := strings.SplitN(v, "=", 2)
		if param[0] == "" || param[1] == "" {
			log.Fatalf("Wrong syntax of '-p' flag. Please use it like this: '-p FOO=BAR'")
		}

		userParameterNames = append(userParameterNames, fmt.Sprintf("${%v}", param[0]))
		userParameterValues = append(userParameterValues, param[1])
	}
}

func (o *postOptions) replaceWithFlags(flagName string, flagValue string) {
	if flagValue == "" {
		log.Fatalf("The selected template is using '%[1]s' parameter, but '%[1]s' flag was not set. Use '-p %[1]s=\"FOOBAR\"' to fix this.", flagName)
	}

	found := false

	if o.limitedSupport.SearchFlag(flagName) {
		found = true
		o.limitedSupport.ReplaceWithFlag(flagName, flagValue)
	}

	if !found {
		log.Fatalf("The selected template is not using '%s' parameter, but '--param' flag was set. Do not use '-p %s=%s' to fix this.", flagName, flagName, flagValue)
	}
}
