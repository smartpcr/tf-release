terraform {
  required_providers {
    labdeploy = {
      source  = "registry.local/smartpcr/labdeploy"
      version = "0.1.0"
    }
  }
}

provider "labdeploy" {
  default_target = {
    transport    = "winrm"
    os           = "windows"
    username     = "LAB\\deploy-svc"
    password_env = "LABDEPLOY_PASSWORD" # env var NAME on the runner
  }
}

variable "app_version" {
  type        = string
  description = "Artifact version to roll out (pipeline sets this)."
}

variable "app_checksum" {
  type        = string
  description = "sha256:<hex> of the artifact for var.app_version."
}

# ---------------------------------------------------------------------------
# Deployment: windows service on one lab host
# ---------------------------------------------------------------------------
resource "labdeploy_deployment" "sample_svc" {
  spec_file        = "${path.module}/specs/windows-service.yaml"
  version_override = var.app_version
  variables = {
    HOST     = "lab-win-01.contoso.lab"
    CHECKSUM = var.app_checksum
    FEED_URL = "https://pkgs.dev.azure.com/org/proj/_packaging/lab/nuget/v3/index.json"
  }
  destroy_mode = "purge"
}

# ---------------------------------------------------------------------------
# E2E tests: re-run whenever the deployment's spec or version changes
# ---------------------------------------------------------------------------
resource "labdeploy_e2e_test" "sample_svc" {
  spec_file = "${path.module}/specs/e2e-testrun.yaml"
  variables = {
    HOST     = "lab-win-01.contoso.lab"
    CHECKSUM = var.app_checksum
  }
  triggers = {
    deployment = labdeploy_deployment.sample_svc.spec_hash
    version    = labdeploy_deployment.sample_svc.deployed_version
  }
  fail_on_test_failure = true
  depends_on           = [labdeploy_deployment.sample_svc]
}

output "deployed_version" { value = labdeploy_deployment.sample_svc.deployed_version }
output "service_status"   { value = labdeploy_deployment.sample_svc.service_status }
output "tests_passed"     { value = labdeploy_e2e_test.sample_svc.passed }
output "test_summary"     { value = labdeploy_e2e_test.sample_svc.summary }
