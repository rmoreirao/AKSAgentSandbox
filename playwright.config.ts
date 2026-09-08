import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './tests/ui',
  testMatch: '**/*.spec.ts',
  fullyParallel: false,
  forbidOnly: true,
  retries: 0,
  reporter: 'line',
  outputDir: 'test-results',
  use: {
    ignoreHTTPSErrors: process.env.DEVSANDBOX_SKIP_TLS_VERIFY === 'true',
    screenshot: 'only-on-failure',
    // The test starts traces only after the credential fragment has been removed,
    // so failure artifacts cannot persist the one-time URL.
    trace: 'off',
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        launchOptions: process.env.DEVSANDBOX_GATEWAY_IP
          ? {
              args: [
                `--host-resolver-rules=MAP api.devsandbox.invalid ${process.env.DEVSANDBOX_GATEWAY_IP},MAP sandbox.devsandbox.invalid ${process.env.DEVSANDBOX_GATEWAY_IP}`,
              ],
            }
          : undefined,
      },
    },
  ],
});
