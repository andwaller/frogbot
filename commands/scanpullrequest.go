package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jfrog/frogbot/commands/utils"
	"github.com/jfrog/froggit-go/vcsclient"
	audit "github.com/jfrog/jfrog-cli-core/v2/xray/commands/audit/generic"
	xrayutils "github.com/jfrog/jfrog-cli-core/v2/xray/utils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	clientLog "github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/jfrog/jfrog-client-go/xray/services"
	clitool "github.com/urfave/cli/v2"
)

func ScanPullRequest(c *clitool.Context) error {
	params, client, err := utils.GetParamsAndClient()
	if err != nil {
		return err
	}

	if c.Bool("use-labels") {
		if shouldScan, err := handleFrogbotLabel(params, client); err != nil || !shouldScan {
			return err
		}
	}

	return scanPullRequest(params, client)
}

func GetScanPullRequestFlags() []clitool.Flag {
	return []clitool.Flag{
		&clitool.BoolFlag{
			Name:    "use-labels",
			Usage:   "Set to true if scan-pull-request is triggered by adding '🐸 frogbot scan' label to a pull request.",
			EnvVars: []string{"JF_USE_LABELS"},
		},
	}
}

// Run before scan, to make sure the Xray scan will be run only after adding the 'frogbot scan pr' label.
// If label is missing - create the label and do nothing
// If pr isn't labeled - do nothing
// If pr is labeled - remove label and allow running Xray scan (return nil)
// params - Frogbot parameters retrieved from the environment variables
// client - The VCS client
func handleFrogbotLabel(params *utils.FrogbotParams, client vcsclient.VcsClient) (bool, error) {
	labelInfo, err := client.GetLabel(context.Background(), params.RepoOwner, params.Repo, string(utils.LabelName))
	if err != nil {
		return false, err
	}
	if labelInfo == nil {
		clientLog.Info("Creating label " + string(utils.LabelName))
		err = client.CreateLabel(context.Background(), params.RepoOwner, params.Repo, vcsclient.LabelInfo{
			Name:        string(utils.LabelName),
			Description: string(utils.LabelDescription),
			Color:       string(utils.LabelColor),
		})
		if err != nil {
			return false, err
		}
		clientLog.Info(fmt.Sprintf("label '%s' was created. Please label this pull request to trigger an Xray scan", string(utils.LabelName)))
		return false, nil
	}

	labels, err := client.ListPullRequestLabels(context.Background(), params.RepoOwner, params.Repo, params.PullRequestID)
	if err != nil {
		return false, err
	}
	clientLog.Debug("The following labels were found in the pull request: ", labels)
	for _, label := range labels {
		if label != string(utils.LabelName) {
			continue
		}
		clientLog.Info("Unlabeling '"+utils.LabelName+"' from pull request", params.PullRequestID)
		err := client.UnlabelPullRequest(context.Background(), params.RepoOwner, params.Repo, string(utils.LabelName), params.PullRequestID)
		return err == nil, err
	}
	return false, fmt.Errorf("please add the '%s' label to trigger an Xray scan", string(utils.LabelName))
}

// Scan a pull request by as follows:
// a. Audit the depedencies of the source and the target branches.
// b. Compare the vulenrabilities found in source and target branches, and show only the new vulnerabilities added by the pull request.
func scanPullRequest(params *utils.FrogbotParams, client vcsclient.VcsClient) error {
	// Audit PR code
	xrayScanParams := createXrayScanParams(params.Watches, params.Project)
	currentScan, err := auditSource(xrayScanParams, params)
	if err != nil {
		return err
	}

	// Audit target code
	previousScan, err := auditTarget(client, xrayScanParams, params)
	if err != nil {
		return err
	}
	clientLog.Info("Xray scan completed")

	// Comment frogbot message on the PR
	message := createPullRequestMessage(createVulnerabilitiesRows(previousScan, currentScan))
	return client.AddPullRequestComment(context.Background(), params.RepoOwner, params.Repo, message, params.PullRequestID)
}

// Create vulnerabilities rows. The rows should contain only the new issues added by this PR
func createVulnerabilitiesRows(previousScan, currentScan []services.ScanResponse) []xrayutils.VulnerabilityRow {
	var vulnerabilitiesRows []xrayutils.VulnerabilityRow
	for i := 0; i < len(currentScan); i += 1 {
		if len(currentScan[i].Violations) > 0 {
			vulnerabilitiesRows = append(vulnerabilitiesRows, getNewViolations(previousScan[i], currentScan[i])...)
		} else if len(currentScan[i].Vulnerabilities) > 0 {
			vulnerabilitiesRows = append(vulnerabilitiesRows, getNewVulnerabilities(previousScan[i], currentScan[i])...)
		}
	}
	return vulnerabilitiesRows
}

func createXrayScanParams(watches, project string) (params services.XrayGraphScanParams) {
	params.ScanType = services.Dependency
	params.IncludeLicenses = false
	if watches != "" {
		params.Watches = strings.Split(watches, utils.WatchesDelimiter)
		return
	}
	if project != "" {
		params.ProjectKey = project
		return
	}
	// No context was supplied, request from Xray to return all known vulnerabilities.
	params.IncludeVulnerabilities = true
	return
}

func auditSource(xrayScanParams services.XrayGraphScanParams, params *utils.FrogbotParams) ([]services.ScanResponse, error) {
	wd, err := os.Getwd()
	if err != nil {
		return []services.ScanResponse{}, err
	}
	clientLog.Info("Auditing " + wd)
	return runInstallAndAudit(xrayScanParams, params, wd, true)
}

func auditTarget(client vcsclient.VcsClient, xrayScanParams services.XrayGraphScanParams, params *utils.FrogbotParams) (res []services.ScanResponse, err error) {
	clientLog.Info("Auditing " + params.Repo + " " + params.BaseBranch)
	// First download the target repo to temp dir
	tempWorkdir, err := fileutils.CreateTempDir()
	if err != nil {
		return
	}
	clientLog.Debug("Created temp working directory: " + tempWorkdir)
	defer fileutils.RemoveTempDir(tempWorkdir)
	clientLog.Debug(fmt.Sprintf("Downloading %s/%s , branch:%s to:%s", params.RepoOwner, params.Repo, params.BaseBranch, tempWorkdir))
	err = client.DownloadRepository(context.Background(), params.RepoOwner, params.Repo, params.BaseBranch, tempWorkdir)
	if err != nil {
		return
	}
	clientLog.Debug("Downloaded target repository")
	return runInstallAndAudit(xrayScanParams, params, tempWorkdir, false)
}

func runInstallAndAudit(xrayScanParams services.XrayGraphScanParams, params *utils.FrogbotParams, workDir string, failOnInstallationErrors bool) ([]services.ScanResponse, error) {
	restoreDir, err := utils.Chdir(workDir)
	if err != nil {
		return []services.ScanResponse{}, err
	}
	defer restoreDir()
	if err = runInstallIfNeeded(params, workDir, failOnInstallationErrors); err != nil {
		return []services.ScanResponse{}, err
	}
	results, _, err := audit.GenericAudit(xrayScanParams, &params.Server, false, false, false, []string{})
	return results, err
}

func runInstallIfNeeded(params *utils.FrogbotParams, workDir string, failOnInstallationErrors bool) error {
	if params.InstallCommandName == "" {
		return nil
	}
	clientLog.Info("Executing '"+params.InstallCommandName+"'", params.InstallCommandArgs, "at ", workDir)
	//#nosec G204 -- False positive - the subprocess only run after the user's approval.
	if err := exec.Command(params.InstallCommandName, params.InstallCommandArgs...).Run(); err != nil {
		if failOnInstallationErrors {
			return err
		}
		clientLog.Info("Couldn't run the installation command on the base branch. Assuming new project in the source branch: " + err.Error())
		return nil
	}
	return nil
}

func getNewViolations(previousScan, currentScan services.ScanResponse) (newViolationsRows []xrayutils.VulnerabilityRow) {
	existsViolationsMap := make(map[string]xrayutils.VulnerabilityRow)
	violationsRows, _, _, err := xrayutils.PrepareViolationsTable(previousScan.Violations, false, false)
	if err != nil {
		return
	}
	for _, violation := range violationsRows {
		existsViolationsMap[getUniqueID(violation)] = violation
	}
	violationsRows, _, _, err = xrayutils.PrepareViolationsTable(currentScan.Violations, false, false)
	if err != nil {
		return
	}
	for _, violation := range violationsRows {
		if _, exists := existsViolationsMap[getUniqueID(violation)]; !exists {
			newViolationsRows = append(newViolationsRows, violation)
		}
	}
	return
}

func getNewVulnerabilities(previousScan, currentScan services.ScanResponse) (newVulnerabilitiesRows []xrayutils.VulnerabilityRow) {
	existsVulnerabilitiesMap := make(map[string]xrayutils.VulnerabilityRow)
	vulnerabilitiesRows, err := xrayutils.PrepareVulnerabilitiesTable(previousScan.Vulnerabilities, false, false)
	if err != nil {
		return
	}
	for _, vulnerability := range vulnerabilitiesRows {
		existsVulnerabilitiesMap[getUniqueID(vulnerability)] = vulnerability
	}
	vulnerabilitiesRows, err = xrayutils.PrepareVulnerabilitiesTable(currentScan.Vulnerabilities, false, false)
	if err != nil {
		return
	}
	for _, vulnerability := range vulnerabilitiesRows {
		if _, exists := existsVulnerabilitiesMap[getUniqueID(vulnerability)]; !exists {
			newVulnerabilitiesRows = append(newVulnerabilitiesRows, vulnerability)
		}
	}
	return
}

func getUniqueID(vulnerability xrayutils.VulnerabilityRow) string {
	return vulnerability.ImpactedPackageName + vulnerability.ImpactedPackageVersion + vulnerability.IssueId
}

func createPullRequestMessage(vulnerabilitiesRows []xrayutils.VulnerabilityRow) string {
	if len(vulnerabilitiesRows) == 0 {
		return utils.GetBanner(utils.NoVulnerabilityBannerSource) + utils.WhatIsFrogbotMd
	}
	var tableContent string
	for _, vulnerability := range vulnerabilitiesRows {
		var componentName, componentVersion, cve string
		if len(vulnerability.Components) > 0 {
			componentName = vulnerability.Components[0].Name
			componentVersion = vulnerability.Components[0].Version
		}
		if len(vulnerability.Cves) > 0 {
			cve = vulnerability.Cves[0].Id
		}
		tableContent += fmt.Sprintf("\n| %s | %s | %s | %s | %s | %s | %s ", utils.GetSeverityTag(utils.IconName(vulnerability.Severity))+" "+vulnerability.Severity, vulnerability.ImpactedPackageName,
			vulnerability.ImpactedPackageVersion, vulnerability.FixedVersions, componentName, componentVersion, cve)
	}
	return utils.GetBanner(utils.VulnerabilitiesBannerSource) + utils.WhatIsFrogbotMd + utils.TableHeder + tableContent
}
