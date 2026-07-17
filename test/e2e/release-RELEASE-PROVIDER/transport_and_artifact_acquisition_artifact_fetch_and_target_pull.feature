@story-release:RELEASE-PROVIDER @phase-transport-and-artifact-acquisition @stage-artifact-fetch-and-target-pull @setup-inline
Feature: Artifact Fetch and Target Pull
  Fetch streams an artifact into a runner temp file while verifying its sha256,
  surfacing ERR_ARTIFACT_FETCH (including the HTTP status) when the origin
  returns 404. For nuget_feed sources the flat-container download URL and the
  generated target-pull scripts must match the committed goldens under
  internal/artifact/testdata.

  Scenario: Fetch sha verify and 404
    Given an httptest server serving a zip with a correct sha256 and a 404 route
    When Fetch runs against the good route
    Then the fetched sha256 matches and the artifact is spooled to a temp file
    When Fetch runs against the 404 route
    Then it fails with ERR_ARTIFACT_FETCH including 404

  Scenario: NuGet URL and target-pull script
    Given a nuget_feed source artifact
    When the download URL is built and the target-pull scripts are generated
    Then the URL matches the flat-container form and the scripts match the committed golden
