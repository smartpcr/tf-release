@story-release:RELEASE-PROVIDER @phase-transport-and-artifact-acquisition @stage-powershell-encoding-and-winrm-transport @setup-inline
Feature: PowerShell EncodedCommand encoding and WinRM upload chunk math
  As the release provider's WinRM transport
  I must encode PowerShell commands to exact, stable bytes and split uploads
  into deterministic chunks so remote execution and file transfer are correct.

  Scenario: EncodedCommand exact bytes
    Given the known PowerShell script and env containing "O'Brien"
    When the command is PowerShell-encoded
    Then the encoded bytes match the committed golden fixture "encoded_command.golden"
    And the single-quote escaping in the decoded payload is correct

  Scenario: Chunk math boundaries
    Given upload payloads of sizes 0, 1, 48000 and 48001 bytes
    When the upload is chunked
    Then the chunk counts and offsets match the committed golden fixture "chunk_plan.golden.json"
