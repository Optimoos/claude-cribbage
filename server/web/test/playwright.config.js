// Browser tests for the web client (server/web/static) against a real
// cribserver. Each test starts its own server; see server.js.
const { defineConfig } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  globalSetup: require.resolve('./build.js'),
  timeout: 60000,
  expect: { timeout: 15000 },
  workers: 4,
  reporter: process.env.CI ? 'github' : 'list',
  use: { browserName: 'chromium', trace: 'retain-on-failure' },
});
