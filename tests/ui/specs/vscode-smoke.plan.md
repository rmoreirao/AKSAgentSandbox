# VS Code Smoke Test Plan

## Application Overview

DevSandbox exchanges an owner-authenticated, fragment-based one-time URL for a host-only session and then proxies browser-based code-server through the web gateway and Sandbox Router. The test URL is supplied only through `DEVSANDBOX_VSCODE_URL` and is never written to source or output.

Live planning exploration is blocked until a deployed URL is available. The generated scenario uses code-server's accessibility contract and must be healed with `playwright-cli` snapshots against the deployment before claiming a live pass.

## Test Scenarios

### 1. VS Code browser experience

**Seed:** The first navigation in `tests/ui/vscode-smoke.spec.ts` opens the environment-provided one-time URL in a fresh Chromium context.

#### 1.1. owner-opens-repository-and-terminal

**File:** `tests/ui/vscode-smoke.spec.ts`

**Steps:**
  1. Open the owner-authenticated one-time VS Code URL.
    - expect: code-server loads from a clean URL with no fragment.
    - expect: the Explorer control is visible.
  2. Open Explorer.
    - expect: the repository root is visible in the Explorer tree.
  3. Open a new integrated terminal and run `pwd`.
    - expect: the terminal reports `/workspace/repo`.
  4. Open the original one-time URL in a new browser context.
    - expect: the fragment is removed.
    - expect: the gateway reports that the link is invalid or expired.
