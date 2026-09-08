// spec: tests/ui/specs/vscode-smoke.plan.md
import { expect, test, type BrowserContext, type Page, type TestInfo } from '@playwright/test';

const oneTimeURL = process.env.DEVSANDBOX_VSCODE_URL;
const repositoryName = process.env.DEVSANDBOX_VSCODE_REPOSITORY ?? 'repo';
const escapedRepositoryName = repositoryName.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

async function openSensitiveURL(page: Page, target: string): Promise<void> {
  const expectedHost = new URL(target).host;
  await page.goto('about:blank');
  await page.evaluate((url) => window.location.replace(url), target).catch((error: unknown) => {
    if (!String(error).includes('Execution context was destroyed')) {
      throw error;
    }
  });
  await page.waitForURL((url) => url.host === expectedHost && url.hash === '');
  await page.waitForFunction(
    () =>
      window.location.pathname !== '/bootstrap' ||
      document.body.textContent?.includes('Invalid or expired link'),
  );
}

async function traceCleanPage(
  context: BrowserContext,
  testInfo: TestInfo,
  body: () => Promise<void>,
): Promise<void> {
  await context.tracing.start({ screenshots: true, snapshots: true, sources: true });
  try {
    await body();
    await context.tracing.stop();
  } catch (error) {
    await context.tracing.stop({ path: testInfo.outputPath('trace.zip') }).catch(() => {});
    throw error;
  }
}

test.describe('VS Code browser experience', () => {
  test('owner opens repository and terminal', async ({ browser, page }, testInfo) => {
    test.setTimeout(60_000);
    test.skip(!oneTimeURL, 'Blocked: DEVSANDBOX_VSCODE_URL is required for the deployed UI scenario');

    // 1. Open the owner-authenticated one-time VS Code URL.
    await openSensitiveURL(page, oneTimeURL!);
    await traceCleanPage(page.context(), testInfo, async () => {
      await expect(page).toHaveURL((url) => url.hash === '' && url.pathname !== '/bootstrap');
      await expect(page.getByRole('button', { name: /Explorer/i }).first()).toBeVisible();

      // 2. Open Explorer.
      await page.getByRole('button', { name: /Explorer/i }).first().click();
      await expect(
        page.getByRole('button', {
          name: new RegExp(`^Explorer Section: ${escapedRepositoryName}$`, 'i'),
        }),
      ).toBeVisible();

      // 3. Open a new integrated terminal and run `pwd`.
      await page.keyboard.press('Control+Shift+Backquote');
      const trustFolder = page.getByRole('button', { name: 'Trust Folder & Continue', exact: true });
      if (await trustFolder.isVisible()) {
        await trustFolder.click();
      }
      const terminalInput = page.getByRole('textbox', { name: /Terminal (?:input|1)/i });
      await expect(terminalInput).toBeVisible();
      await terminalInput.pressSequentially('pwd');
      await terminalInput.press('Enter');
      await expect(terminalInput).toBeVisible();
    });

    // 4. Open the original one-time URL in a new browser context.
    const reuseContext = await browser.newContext();
    try {
      const reusedPage = await reuseContext.newPage();
      await openSensitiveURL(reusedPage, oneTimeURL!);
      await traceCleanPage(reuseContext, testInfo, async () => {
        await expect(reusedPage).toHaveURL((url) => url.hash === '');
        await expect(reusedPage.getByText('Invalid or expired link', { exact: true })).toBeVisible();
      });
    } finally {
      await reuseContext.close();
    }
  });
});
