// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

//go:build integration

package integration

import (
	"archive/zip"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/elastic-agent-libs/kibana"
	"github.com/elastic/elastic-agent/internal/pkg/agent/application/paths"
	"github.com/elastic/elastic-agent/pkg/control/v2/client"
	"github.com/elastic/elastic-agent/pkg/control/v2/cproto"
	atesting "github.com/elastic/elastic-agent/pkg/testing"
	"github.com/elastic/elastic-agent/pkg/testing/define"
	"github.com/elastic/elastic-agent/pkg/testing/tools"
	"github.com/elastic/elastic-agent/pkg/testing/tools/fleettools"
	"github.com/elastic/elastic-agent/pkg/testing/tools/testcontext"
)

const (
	// TODO: Setup a GitHub Action to update this for each release of https://github.com/elastic/endpoint-package
	endpointPackageVersion       = "8.11.0"
	endpointHealthPollingTimeout = 2 * time.Minute
)

//go:embed endpoint_security_package.json.tmpl
var endpointPackagePolicyTemplate string

type endpointPackageTemplateVars struct {
	ID       string
	Name     string
	PolicyID string
	Version  string
}

var protectionTests = []struct {
	name      string
	protected bool
}{
	{
		name: "unprotected",
	},
	{
		name:      "protected",
		protected: true,
	},
}

// Tests that the agent can install and uninstall the endpoint-security service while remaining
// healthy.
//
// Installing endpoint-security requires a Fleet managed agent with the Elastic Defend integration
// installed. The endpoint-security service is uninstalled when the agent is uninstalled.
//
// The agent is automatically uninstalled as part of test cleanup when installed with
// fixture.Install via tools.InstallAgentWithPolicy. Failure to uninstall the agent will fail the
// test automatically.
func TestInstallAndCLIUninstallWithEndpointSecurity(t *testing.T) {
	info := define.Require(t, define.Requirements{
		Group: Fleet,
		Stack: &define.Stack{},
		Local: false, // requires Agent installation
		Sudo:  true,  // requires Agent installation
		OS: []define.OS{
			{Type: define.Linux},
		},
	})

	for _, tc := range protectionTests {
		t.Run(tc.name, func(t *testing.T) {
			testInstallAndCLIUninstallWithEndpointSecurity(t, info, tc.protected)
		})
	}
}

// Tests that the agent can install and uninstall the endpoint-security service while remaining
// healthy. In this case endpoint-security is uninstalled because the agent was unenrolled, which
// triggers the creation of an empty agent policy removing all inputs (only when not force
// unenrolling). The empty agent policy triggers the uninstall of endpoint because endpoint was
// removed from the policy.
//
// Like the CLI uninstall test, the agent is uninstalled from the command line at the end of the test
// but at this point endpoint is already uninstalled.
func TestInstallAndUnenrollWithEndpointSecurity(t *testing.T) {
	info := define.Require(t, define.Requirements{
		Group: Fleet,
		Stack: &define.Stack{},
		Local: false, // requires Agent installation
		Sudo:  true,  // requires Agent installation
		OS: []define.OS{
			{Type: define.Linux},
		},
	})

	for _, tc := range protectionTests {
		t.Run(tc.name, func(t *testing.T) {
			testInstallAndUnenrollWithEndpointSecurity(t, info, tc.protected)
		})
	}
}

// Tests that the agent can install and uninstall the endpoint-security service
// after the Elastic Defend integration was removed from the policy
// while remaining healthy.
//
// Installing endpoint-security requires a Fleet managed agent with the Elastic Defend integration
// installed. The endpoint-security service is uninstalled the Elastic Defend integration was removed from the policy.
//
// Like the CLI uninstall test, the agent is uninstalled from the command line at the end of the test
// but at this point endpoint should be already uninstalled.

func TestInstallWithEndpointSecurityAndRemoveEndpointIntegration(t *testing.T) {
	info := define.Require(t, define.Requirements{
		Group: Fleet,
		Stack: &define.Stack{},
		Local: false, // requires Agent installation
		Sudo:  true,  // requires Agent installation
		OS: []define.OS{
			{Type: define.Linux},
		},
	})

	for _, tc := range protectionTests {
		t.Run(tc.name, func(t *testing.T) {
			testInstallWithEndpointSecurityAndRemoveEndpointIntegration(t, info, tc.protected)
		})
	}
}

// buildPolicyWithTamperProtection helper function to build the policy request with or without tamper protection
func buildPolicyWithTamperProtection(policy kibana.AgentPolicy, protected bool) kibana.AgentPolicy {
	if protected {
		policy.AgentFeatures = append(policy.AgentFeatures, map[string]interface{}{
			"name":    "tamper_protection",
			"enabled": true,
		})
	}
	policy.IsProtected = protected
	return policy
}

func testInstallAndCLIUninstallWithEndpointSecurity(t *testing.T, info *define.Info, protected bool) {
	deadline := time.Now().Add(10 * time.Minute)
	ctx, cancel := testcontext.WithDeadline(t, context.Background(), deadline)
	defer cancel()

	// Get path to agent executable.
	fixture, err := define.NewFixture(t, define.Version())
	require.NoError(t, err, "could not create agent fixture")

	t.Log("Enrolling the agent in Fleet")
	policyUUID := uuid.New().String()

	createPolicyReq := buildPolicyWithTamperProtection(
		kibana.AgentPolicy{
			Name:        "test-policy-" + policyUUID,
			Namespace:   "default",
			Description: "Test policy " + policyUUID,
			MonitoringEnabled: []kibana.MonitoringEnabledOption{
				kibana.MonitoringEnabledLogs,
				kibana.MonitoringEnabledMetrics,
			},
		},
		protected,
	)

	installOpts := atesting.InstallOpts{
		NonInteractive: true,
		Force:          true,
		Unprivileged:   atesting.NewBool(false),
	}

	policy, err := tools.InstallAgentWithPolicy(ctx, t,
		installOpts, fixture, info.KibanaClient, createPolicyReq)
	require.NoError(t, err, "failed to install agent with policy")

	t.Cleanup(func() {
		t.Log("Un-enrolling Elastic Agent...")
		// Use a separate context as the one in the test body will have been cancelled at this point.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		assert.NoError(t, fleettools.UnEnrollAgent(cleanupCtx, info.KibanaClient, policy.ID))
	})

	t.Log("Installing Elastic Defend")
	pkgPolicyResp, err := installElasticDefendPackage(t, info, policy.ID)
	require.NoErrorf(t, err, "Policy Response was: %v", pkgPolicyResp)

	t.Log("Polling for endpoint-security to become Healthy")
	ctx, cancel = context.WithTimeout(ctx, endpointHealthPollingTimeout)
	defer cancel()

	agentClient := fixture.Client()
	err = agentClient.Connect(ctx)
	require.NoError(t, err, "could not connect to local agent")

	require.Eventually(t,
		func() bool { return agentAndEndpointAreHealthy(t, ctx, agentClient) },
		endpointHealthPollingTimeout,
		time.Second,
		"Endpoint component or units are not healthy.",
	)
	t.Log("Verified endpoint component and units are healthy")
}

func testInstallAndUnenrollWithEndpointSecurity(t *testing.T, info *define.Info, protected bool) {
	// Get path to agent executable.
	fixture, err := define.NewFixture(t, define.Version())
	require.NoError(t, err)

	t.Log("Enrolling the agent in Fleet")
	policyUUID := uuid.New().String()
	createPolicyReq := buildPolicyWithTamperProtection(
		kibana.AgentPolicy{
			Name:        "test-policy-" + policyUUID,
			Namespace:   "default",
			Description: "Test policy " + policyUUID,
			MonitoringEnabled: []kibana.MonitoringEnabledOption{
				kibana.MonitoringEnabledLogs,
				kibana.MonitoringEnabledMetrics,
			},
		},
		protected,
	)

	installOpts := atesting.InstallOpts{
		NonInteractive: true,
		Force:          true,
		Unprivileged:   atesting.NewBool(false),
	}

	ctx, cn := testcontext.WithDeadline(t, context.Background(), time.Now().Add(10*time.Minute))
	defer cn()

	policy, err := tools.InstallAgentWithPolicy(ctx, t, installOpts, fixture, info.KibanaClient, createPolicyReq)
	require.NoError(t, err)

	t.Log("Installing Elastic Defend")
	_, err = installElasticDefendPackage(t, info, policy.ID)
	require.NoError(t, err)

	t.Log("Polling for endpoint-security to become Healthy")
	ctx, cancel := context.WithTimeout(context.Background(), endpointHealthPollingTimeout)
	defer cancel()

	agentClient := fixture.Client()
	err = agentClient.Connect(ctx)
	require.NoError(t, err)

	require.Eventually(t,
		func() bool { return agentAndEndpointAreHealthy(t, ctx, agentClient) },
		endpointHealthPollingTimeout,
		time.Second,
		"Endpoint component or units are not healthy.",
	)
	t.Log("Verified endpoint component and units are healthy")

	// Unenroll the agent
	t.Log("Unenrolling the agent")

	hostname, err := os.Hostname()
	require.NoError(t, err)

	agentID, err := fleettools.GetAgentIDByHostname(ctx, info.KibanaClient, policy.ID, hostname)
	require.NoError(t, err)

	_, err = info.KibanaClient.UnEnrollAgent(ctx, kibana.UnEnrollAgentRequest{ID: agentID})
	require.NoError(t, err)

	t.Log("Waiting for inputs to stop")
	require.Eventually(t,
		func() bool {
			state, err := agentClient.State(ctx)
			if err != nil {
				t.Logf("Error getting agent state: %s", err)
				return false
			}

			if state.State != client.Healthy {
				t.Logf("Agent is not Healthy\n%+v", state)
				return false
			}

			if len(state.Components) != 0 {
				t.Logf("Components have not been stopped and uninstalled!\n%+v", state)
				return false
			}

			if state.FleetState != client.Failed {
				t.Logf("Fleet state has not been marked as failed yet!\n%+v", state)
				return false
			}

			return true
		},
		endpointHealthPollingTimeout,
		time.Second,
		"All components not removed.",
	)
	t.Log("Verified endpoint component and units are removed")

	// Verify that the Endpoint directory was correctly removed.
	// Regression test for https://github.com/elastic/elastic-agent/issues/3077
	agentInstallPath := fixture.WorkDir()
	files, err := os.ReadDir(filepath.Clean(filepath.Join(agentInstallPath, "..")))
	require.NoError(t, err)

	t.Logf("Checking directories at install path %s", agentInstallPath)
	for _, f := range files {
		if !f.IsDir() {
			continue
		}

		t.Log("Found directory", f.Name())
		require.False(t, strings.Contains(f.Name(), "Endpoint"), "Endpoint directory was not removed")
	}
}

func testInstallWithEndpointSecurityAndRemoveEndpointIntegration(t *testing.T, info *define.Info, protected bool) {
	// Get path to agent executable.
	fixture, err := define.NewFixture(t, define.Version())
	require.NoError(t, err)

	t.Log("Enrolling the agent in Fleet")
	policyUUID := uuid.New().String()
	createPolicyReq := buildPolicyWithTamperProtection(
		kibana.AgentPolicy{
			Name:        "test-policy-" + policyUUID,
			Namespace:   "default",
			Description: "Test policy " + policyUUID,
			MonitoringEnabled: []kibana.MonitoringEnabledOption{
				kibana.MonitoringEnabledLogs,
				kibana.MonitoringEnabledMetrics,
			},
		},
		protected,
	)

	installOpts := atesting.InstallOpts{
		NonInteractive: true,
		Force:          true,
		Unprivileged:   atesting.NewBool(false),
	}

	ctx, cn := testcontext.WithDeadline(t, context.Background(), time.Now().Add(10*time.Minute))
	defer cn()

	policy, err := tools.InstallAgentWithPolicy(ctx, t, installOpts, fixture, info.KibanaClient, createPolicyReq)
	require.NoError(t, err)

	t.Log("Installing Elastic Defend")
	pkgPolicyResp, err := installElasticDefendPackage(t, info, policy.ID)
	require.NoErrorf(t, err, "Policy Response was: %#v", pkgPolicyResp)

	t.Log("Polling for endpoint-security to become Healthy")
	ctx, cancel := context.WithTimeout(context.Background(), endpointHealthPollingTimeout)
	defer cancel()

	agentClient := fixture.Client()
	err = agentClient.Connect(ctx)
	require.NoError(t, err)

	require.Eventually(t,
		func() bool { return agentAndEndpointAreHealthy(t, ctx, agentClient) },
		endpointHealthPollingTimeout,
		time.Second,
		"Endpoint component or units are not healthy.",
	)
	t.Log("Verified endpoint component and units are healthy")

	t.Logf("Removing Elastic Defend: %v", fmt.Sprintf("/api/fleet/package_policies/%v", pkgPolicyResp.Item.ID))
	_, err = info.KibanaClient.DeleteFleetPackage(ctx, pkgPolicyResp.Item.ID)
	require.NoError(t, err)

	t.Log("Waiting for endpoint to stop")
	require.Eventually(t,
		func() bool { return agentIsHealthyNoEndpoint(t, ctx, agentClient) },
		endpointHealthPollingTimeout,
		time.Second,
		"Endpoint component or units are still present.",
	)
	t.Log("Verified endpoint component and units are removed")

	// Verify that the Endpoint directory was correctly removed.
	// Regression test for https://github.com/elastic/elastic-agent/issues/3077
	agentInstallPath := fixture.WorkDir()
	files, err := os.ReadDir(filepath.Clean(filepath.Join(agentInstallPath, "..")))
	require.NoError(t, err)

	t.Logf("Checking directories at install path %s", agentInstallPath)
	for _, f := range files {
		if !f.IsDir() {
			continue
		}

		t.Log("Found directory", f.Name())
		// If Endpoint was not currently removed, let's see what was left
		if strings.Contains(f.Name(), "Endpoint") {
			info, err := f.Info()
			if err != nil {
				t.Logf("could not get file info for %q to check what was left"+
					"behind: %v", f.Name(), err)
			}
			ls, err := os.ReadDir(info.Name())
			if err != nil {
				t.Logf("could not list fileson for %q to check what was left"+
					"behind: %v", f.Name(), err)
			}
			var dirEntries []string
			for _, de := range ls {
				dirEntries = append(dirEntries, de.Name())
			}

			if len(dirEntries) == 0 {
				t.Fatalf("Endpoint directory was not removed, but it's empty")
			}
			t.Fatalf("Endpoint directory was not removed, the directory content is: %s",
				strings.Join(dirEntries, ", "))
		}
	}
}

// This is a subset of kibana.AgentPolicyUpdateRequest, using until elastic-agent-libs PR https://github.com/elastic/elastic-agent-libs/pull/141 is merged
// TODO: replace with the elastic-agent-libs when available
type agentPolicyUpdateRequest struct {
	// Name of the policy. Required in an update request.
	Name string `json:"name"`
	// Namespace of the policy. Required in an update request.
	Namespace   string `json:"namespace"`
	IsProtected bool   `json:"is_protected"`
}

// Installs the Elastic Defend package to cause the agent to install the endpoint-security service.
func installElasticDefendPackage(t *testing.T, info *define.Info, policyID string) (r kibana.PackagePolicyResponse, err error) {
	t.Helper()

	t.Log("Templating endpoint package policy request")
	tmpl, err := template.New("pkgpolicy").Parse(endpointPackagePolicyTemplate)
	if err != nil {
		return r, fmt.Errorf("error creating new template: %w", err)
	}

	packagePolicyID := uuid.New().String()
	var pkgPolicyBuf bytes.Buffer

	// Need unique name for Endpoint integration otherwise on multiple runs on the same instance you get
	// http error response with code 409: {StatusCode:409 Error:Conflict Message:An integration policy with the name Defend-cbomziz4uvn5fov9t1gsrcvdwn2p1s7tefnvgsye already exists. Please rename it or choose a different name.}
	err = tmpl.Execute(&pkgPolicyBuf, endpointPackageTemplateVars{
		ID:       packagePolicyID,
		Name:     "Defend-" + packagePolicyID,
		PolicyID: policyID,
		Version:  endpointPackageVersion,
	})
	if err != nil {
		return r, fmt.Errorf("error executing template: %w", err)
	}

	// Make sure the templated value is actually valid JSON before making the API request.
	// Using json.Unmarshal will give us the actual syntax error, calling json.Valid() would not.
	packagePolicyReq := kibana.PackagePolicyRequest{}
	err = json.Unmarshal(pkgPolicyBuf.Bytes(), &packagePolicyReq)
	if err != nil {
		return r, fmt.Errorf("templated package policy is not valid JSON: %s, %w", pkgPolicyBuf.String(), err)
	}

	t.Log("POST /api/fleet/package_policies")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pkgResp, err := info.KibanaClient.InstallFleetPackage(ctx, packagePolicyReq)
	if err != nil {
		t.Logf("Error installing fleet package: %v", err)
		return r, fmt.Errorf("error installing fleet package: %w", err)
	}
	t.Logf("Endpoint package Policy Response:\n%+v", pkgResp)
	return pkgResp, err
}

// Tests that install of Elastic Defend fails if Agent is installed in a base
// path other than default
func TestEndpointSecurityNonDefaultBasePath(t *testing.T) {
	info := define.Require(t, define.Requirements{
		Group: Fleet,
		Stack: &define.Stack{},
		Local: false, // requires Agent installation
		Sudo:  true,  // requires Agent installation
	})

	ctx, cn := testcontext.WithDeadline(t, context.Background(), time.Now().Add(10*time.Minute))
	defer cn()

	// Get path to agent executable.
	fixture, err := define.NewFixture(t, define.Version())
	require.NoError(t, err)

	t.Log("Enrolling the agent in Fleet")
	policyUUID := uuid.New().String()
	createPolicyReq := kibana.AgentPolicy{
		Name:        "test-policy-" + policyUUID,
		Namespace:   "default",
		Description: "Test policy " + policyUUID,
		MonitoringEnabled: []kibana.MonitoringEnabledOption{
			kibana.MonitoringEnabledLogs,
			kibana.MonitoringEnabledMetrics,
		},
	}
	installOpts := atesting.InstallOpts{
		NonInteractive: true,
		Force:          true,
		Unprivileged:   atesting.NewBool(false),
		BasePath:       filepath.Join(paths.DefaultBasePath, "not_default"),
	}
	policyResp, err := tools.InstallAgentWithPolicy(ctx, t, installOpts, fixture, info.KibanaClient, createPolicyReq)
	require.NoErrorf(t, err, "Policy Response was: %v", policyResp)

	t.Log("Installing Elastic Defend")
	pkgPolicyResp, err := installElasticDefendPackage(t, info, policyResp.ID)
	require.NoErrorf(t, err, "Policy Response was: %v", pkgPolicyResp)

	ctx, cancel := testcontext.WithDeadline(t, context.Background(), time.Now().Add(10*time.Minute))
	defer cancel()

	c := fixture.Client()

	require.Eventually(t, func() bool {
		err := c.Connect(ctx)
		if err != nil {
			t.Logf("connecting client to agent: %v", err)
			return false
		}
		defer c.Disconnect()
		state, err := c.State(ctx)
		if err != nil {
			t.Logf("error getting the agent state: %v", err)
			return false
		}
		t.Logf("agent state: %+v", state)
		if state.State != cproto.State_DEGRADED {
			return false
		}
		for _, c := range state.Components {
			if strings.Contains(c.Message,
				"Elastic Defend requires Elastic Agent be installed at the default installation path") {
				return true
			}
		}
		return false
	}, 2*time.Minute, 10*time.Second, "Agent never became DEGRADED with default install message")
}

// Tests that install of Elastic Defend fails if Agent is installed unprivileged.
func TestEndpointSecurityUnprivileged(t *testing.T) {
	info := define.Require(t, define.Requirements{
		Group: Fleet,
		Stack: &define.Stack{},
		Local: false, // requires Agent installation
		Sudo:  true,  // requires Agent installation

		// Only supports Linux at the moment.
		OS: []define.OS{
			{
				Type: define.Linux,
			},
		},
	})

	ctx, cn := testcontext.WithDeadline(t, context.Background(), time.Now().Add(10*time.Minute))
	defer cn()

	// Get path to agent executable.
	fixture, err := define.NewFixture(t, define.Version())
	require.NoError(t, err)

	t.Log("Enrolling the agent in Fleet")
	policyUUID := uuid.New().String()
	createPolicyReq := kibana.AgentPolicy{
		Name:        "test-policy-" + policyUUID,
		Namespace:   "default",
		Description: "Test policy " + policyUUID,
		MonitoringEnabled: []kibana.MonitoringEnabledOption{
			kibana.MonitoringEnabledLogs,
			kibana.MonitoringEnabledMetrics,
		},
	}
	installOpts := atesting.InstallOpts{
		NonInteractive: true,
		Force:          true,
		Unprivileged:   atesting.NewBool(true), // ensure always unprivileged
	}
	policyResp, err := tools.InstallAgentWithPolicy(ctx, t, installOpts, fixture, info.KibanaClient, createPolicyReq)
	require.NoErrorf(t, err, "Policy Response was: %v", policyResp)

	t.Log("Installing Elastic Defend")
	pkgPolicyResp, err := installElasticDefendPackage(t, info, policyResp.ID)
	require.NoErrorf(t, err, "Policy Response was: %v", pkgPolicyResp)

	ctx, cancel := testcontext.WithDeadline(t, context.Background(), time.Now().Add(10*time.Minute))
	defer cancel()

	c := fixture.Client()

	errMsg := "Elastic Defend requires Elastic Agent be running as root"
	if runtime.GOOS == define.Windows {
		errMsg = "Elastic Defend requires Elastic Agent be running as Administrator or SYSTEM"
	}
	require.Eventually(t, func() bool {
		err := c.Connect(ctx)
		if err != nil {
			t.Logf("connecting client to agent: %v", err)
			return false
		}
		defer c.Disconnect()
		state, err := c.State(ctx)
		if err != nil {
			t.Logf("error getting the agent state: %v", err)
			return false
		}
		t.Logf("agent state: %+v", state)
		if state.State != cproto.State_DEGRADED {
			return false
		}
		for _, c := range state.Components {
			if strings.Contains(c.Message, errMsg) {
				return true
			}
		}
		return false
	}, 2*time.Minute, 10*time.Second, "Agent never became DEGRADED with root/Administrator install message")
}

// TestEndpointLogsAreCollectedInDiagnostics tests that diagnostics archive contain endpoint logs
func TestEndpointLogsAreCollectedInDiagnostics(t *testing.T) {
	info := define.Require(t, define.Requirements{
		Group: Fleet,
		Stack: &define.Stack{},
		Local: false, // requires Agent installation
		Sudo:  true,  // requires Agent installation
		OS: []define.OS{
			{Type: define.Linux},
		},
	})

	ctx, cn := testcontext.WithDeadline(t, context.Background(), time.Now().Add(10*time.Minute))
	defer cn()

	// Get path to agent executable.
	fixture, err := define.NewFixture(t, define.Version())
	require.NoError(t, err)

	t.Log("Enrolling the agent in Fleet")
	policyUUID := uuid.New().String()
	createPolicyReq := kibana.AgentPolicy{
		Name:        "test-policy-" + policyUUID,
		Namespace:   "default",
		Description: "Test policy " + policyUUID,
		MonitoringEnabled: []kibana.MonitoringEnabledOption{
			kibana.MonitoringEnabledLogs,
			kibana.MonitoringEnabledMetrics,
		},
	}
	installOpts := atesting.InstallOpts{
		NonInteractive: true,
		Force:          true,
		Unprivileged:   atesting.NewBool(false),
	}

	policyResp, err := tools.InstallAgentWithPolicy(ctx, t, installOpts, fixture, info.KibanaClient, createPolicyReq)
	require.NoErrorf(t, err, "Policy Response was: %v", policyResp)

	t.Cleanup(func() {
		t.Log("Un-enrolling Elastic Agent...")
		// Use a separate context as the one in the test body will have been cancelled at this point.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		assert.NoError(t, fleettools.UnEnrollAgent(cleanupCtx, info.KibanaClient, policyResp.ID))
	})

	t.Log("Installing Elastic Defend")
	pkgPolicyResp, err := installElasticDefendPackage(t, info, policyResp.ID)
	require.NoErrorf(t, err, "Policy Response was: %v", pkgPolicyResp)

	// wait for endpoint to be healthy
	t.Log("Polling for endpoint-security to become Healthy")
	pollingCtx, pollingCancel := context.WithTimeout(ctx, endpointHealthPollingTimeout)
	defer pollingCancel()

	require.Eventually(t,
		func() bool {
			agentClient := fixture.Client()
			err = agentClient.Connect(ctx)
			if err != nil {
				t.Logf("error connecting to agent: %v", err)
				return false
			}
			defer agentClient.Disconnect()
			return agentAndEndpointAreHealthy(t, pollingCtx, agentClient)
		},
		endpointHealthPollingTimeout,
		time.Second,
		"Endpoint component or units are not healthy.",
	)

	outDir := t.TempDir()
	diagFile := t.Name() + ".zip"
	diagAbsPath := filepath.Join(outDir, diagFile)
	_, err = fixture.Exec(ctx, []string{"diagnostics", "-f", diagAbsPath})
	require.NoError(t, err, "diagnostics command failed")
	require.FileExists(t, diagAbsPath, "diagnostic archive should have been created")
	checkDiagnosticsForEndpointFiles(t, diagAbsPath)
}

func checkDiagnosticsForEndpointFiles(t *testing.T, diagsPath string) {
	zipReader, err := zip.OpenReader(diagsPath)
	require.NoError(t, err, "error opening diagnostics archive")

	defer func(zipReader *zip.ReadCloser) {
		err := zipReader.Close()
		assert.NoError(t, err, "error closing diagnostic archive")
	}(zipReader)

	t.Logf("---- Contents of diagnostics archive")
	for _, file := range zipReader.File {
		t.Logf("%q - %+v", file.Name, file.FileHeader.FileInfo())
	}
	t.Logf("---- End contents of diagnostics archive")
	// check there are files under the components/ directory
	endpointComponentDirName := "components/endpoint-default"
	endpointComponentDir, err := zipReader.Open(endpointComponentDirName)
	if assert.NoErrorf(t, err, "error looking up directory %q in diagnostic archive: %v", endpointComponentDirName, err) {
		defer func(endpointComponentDir fs.File) {
			err := endpointComponentDir.Close()
			if err != nil {
				assert.NoError(t, err, "error closing endpoint component directory")
			}
		}(endpointComponentDir)
		if assert.Implementsf(t, (*fs.ReadDirFile)(nil), endpointComponentDir, "endpoint should have a directory in the diagnostic archive under %s", endpointComponentDirName) {
			dirFile := endpointComponentDir.(fs.ReadDirFile)
			endpointFiles, err := dirFile.ReadDir(-1)
			assert.NoError(t, err, "error reading endpoint component directory %q in diagnostic archive", endpointComponentDirName)
			assert.NotEmpty(t, endpointFiles, "endpoint component directory should not be empty")
		}
	}

	// check endpoint logs
	servicesLogDirName := "logs/services"
	servicesLogDir, err := zipReader.Open(servicesLogDirName)
	if assert.NoErrorf(t, err, "error looking up directory %q in diagnostic archive: %v", servicesLogDirName, err) {
		defer func(servicesLogDir fs.File) {
			err := servicesLogDir.Close()
			if err != nil {
				assert.NoError(t, err, "error closing services logs directory")
			}
		}(servicesLogDir)
		if assert.Implementsf(t, (*fs.ReadDirFile)(nil), servicesLogDir, "service logs should be in a directory in the diagnostic archive under %s", servicesLogDir) {
			dirFile := servicesLogDir.(fs.ReadDirFile)
			servicesLogFiles, err := dirFile.ReadDir(-1)
			assert.NoError(t, err, "error reading services logs directory %q in diagnostic archive", servicesLogDirName)
			assert.True(t,
				slices.ContainsFunc(servicesLogFiles,
					func(entry fs.DirEntry) bool {
						return strings.HasPrefix(entry.Name(), "endpoint-") && strings.HasSuffix(entry.Name(), ".log")
					}),
				"service logs should contain endpoint-*.log files",
			)
		}
	}
}

func agentAndEndpointAreHealthy(t *testing.T, ctx context.Context, agentClient client.Client) bool {
	t.Helper()

	state, err := agentClient.State(ctx)
	if err != nil {
		t.Logf("Error getting agent state: %s", err)
		return false
	}

	if state.State != client.Healthy {
		t.Logf("local Agent is not Healthy: current state: %+v", state)
		return false
	}

	foundEndpointInputUnit := false
	foundEndpointOutputUnit := false
	for _, comp := range state.Components {
		isEndpointComponent := strings.Contains(comp.Name, "endpoint")
		if comp.State != client.Healthy {
			t.Logf("endpoint component is not Healthy: current state: %+v", comp)
			return false
		}

		for _, unit := range comp.Units {
			if isEndpointComponent {
				if unit.UnitType == client.UnitTypeInput {
					foundEndpointInputUnit = true
				}
				if unit.UnitType == client.UnitTypeOutput {
					foundEndpointOutputUnit = true
				}
			}

			if unit.State != client.Healthy {
				t.Logf("unit %q is not Healthy\n%+v", unit.UnitID, unit)
				return false
			}
		}
	}

	// Ensure both the endpoint input and output units were found and healthy.
	if !foundEndpointInputUnit || !foundEndpointOutputUnit {
		t.Logf("State did not contain endpoint units (input: %v/output: %v) state: %+v. ", foundEndpointInputUnit, foundEndpointOutputUnit, state)
		return false
	}

	return true
}

func agentIsHealthyNoEndpoint(t *testing.T, ctx context.Context, agentClient client.Client) bool {
	t.Helper()

	state, err := agentClient.State(ctx)
	if err != nil {
		t.Logf("Error getting agent state: %s", err)
		return false
	}

	if state.State != client.Healthy {
		t.Logf("Agent is not Healthy\n%+v", state)
		return false
	}

	foundEndpointComponent := false
	foundEndpointInputUnit := false
	foundEndpointOutputUnit := false
	for _, comp := range state.Components {
		isEndpointComponent := strings.Contains(comp.Name, "endpoint")
		if isEndpointComponent {
			foundEndpointComponent = true
		}
		if comp.State != client.Healthy {
			t.Logf("Component is not Healthy\n%+v", comp)
			return false
		}

		for _, unit := range comp.Units {
			if isEndpointComponent {
				if unit.UnitType == client.UnitTypeInput {
					foundEndpointInputUnit = true
				}
				if unit.UnitType == client.UnitTypeOutput {
					foundEndpointOutputUnit = true
				}
			}

			if unit.State != client.Healthy {
				t.Logf("Unit is not Healthy\n%+v", unit)
				return false
			}
		}
	}

	// Ensure both the endpoint input and output units were found and healthy.
	if foundEndpointComponent || foundEndpointInputUnit || foundEndpointOutputUnit {
		t.Logf("State did contain endpoint or endpoint units!\n%+v", state)
		return false
	}

	return true
}
