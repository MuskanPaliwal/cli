package agents

import "testing"

const claudeWorkspaceTrustPane = `Accessing workspace:
/tmp/e2e-repo-1996556193

Quick safety check: Is this a project you created or one you trust?

❯ No, exit
  Yes, I trust this folder

Enter to confirm · Esc to cancel`

func TestClaudeStartupSelectionIsNo_DetectsWorkspaceTrustDefault(t *testing.T) {
	t.Parallel()

	if !claudeStartupSelectionIsNo(claudeWorkspaceTrustPane) {
		t.Fatal("workspace trust dialog defaults to No, so startup must move to Yes before confirming")
	}
}

func TestClaudeStartupSelectionIsNo_IgnoresSelectedYes(t *testing.T) {
	t.Parallel()

	const selectedYes = `  No, exit
❯ Yes, I trust this folder
Enter to confirm · Esc to cancel`
	if claudeStartupSelectionIsNo(selectedYes) {
		t.Fatal("startup must not move down when a Yes option is already selected")
	}
}
