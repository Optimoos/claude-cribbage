// The web client end to end: sign-in by code and by emailed link, account
// deletion by emailed link, signing out while the server is gone, and a
// second user signing in on the same tab.
const { test, expect } = require('./server');

const linkIn = (body, kind) => new RegExp(`(https?://\\S+#${kind}=[0-9a-f]{64})`).exec(body)[1];
const codeIn = body => /^ {4}(\d{4} \d{4})$/m.exec(body)[1];

// emailSignIn asks for a code for name and returns the email it sent.
// The page is loaded afresh unless stay.
async function emailSignIn(page, server, name, to, stay) {
  const before = server.mails(to).length;
  if (!stay) await page.goto(server.url);
  await page.locator('#signin-form input[name=name]').fill(name);
  await page.locator('#signin-form button.btn-primary').click();
  await expect(page.locator('#code-panel')).toBeVisible();
  return server.lastMail(to, before);
}

// newAccount makes an account by code and leaves the page in the lobby.
async function newAccount(page, server, username, stay) {
  const email = `${username}@example.com`;
  await enterCode(page, await emailSignIn(page, server, email, email, stay));
  await page.locator('#username-form input[name=username]').fill(username);
  await page.locator('#username-form button.btn-primary').click();
  await expect(page.locator('#lobby')).toBeVisible();
  return email;
}

async function enterCode(page, body) {
  await page.locator('#signin-code-form input[name=code]').fill(codeIn(body));
  await page.locator('#signin-code-form button.btn-primary').click();
}

async function openAccount(page) {
  await page.locator('#settings-btn').click();
  await page.locator('#account-btn').click();
  await expect(page.locator('#account-dialog')).toBeVisible();
}

test('a sign-in link from the email creates the account', async ({ page, server }) => {
  const body = await emailSignIn(page, server, 'linky@example.com', 'linky@example.com');
  const other = await page.context().browser().newPage(); // as if opened from the mail app
  await other.goto(linkIn(body, 'signin'));
  await expect(other.locator('#link-panel')).toBeVisible();
  await other.locator('#link-confirm').click();
  await other.locator('#username-form input[name=username]').fill('linky');
  await other.locator('#username-form button.btn-primary').click();
  await expect(other.locator('#lobby')).toBeVisible();
  await expect(other.locator('#who')).toContainText('linky', { useInnerText: false });
});

test('a sign-in link signs in an existing account', async ({ page, server, browser }) => {
  const email = await newAccount(page, server, 'alice');
  const ctx = await browser.newContext();
  const body = await emailSignIn(await ctx.newPage(), server, 'alice', email);
  const other = await ctx.newPage();
  await other.goto(linkIn(body, 'signin'));
  await other.locator('#link-confirm').click();
  await expect(other.locator('#lobby')).toBeVisible();
  await expect(other.locator('#who')).toContainText('alice', { useInnerText: false });
});

test('a delete link from the email deletes the account', async ({ page, server, browser }) => {
  const email = await newAccount(page, server, 'bob');
  await openAccount(page);
  const before = server.mails(email).length;
  await page.locator('#delete-form button').click();
  await expect(page.locator('#delete-msg')).toContainText('for a link to confirm');
  const body = await server.lastMail(email, before);
  const other = await (await browser.newContext()).newPage();
  await other.goto(linkIn(body, 'delete'));
  await expect(other.locator('#delete-panel')).toBeVisible();
  await other.locator('#delete-confirm').click();
  await expect(other.locator('#login-error')).toContainText('The account bob was deleted.');
  // The name is free again: a new sign-in for it is a new account.
  const again = await emailSignIn(await (await browser.newContext()).newPage(), server, email, email);
  expect(again).toContain('create a cribbage account');
});

test('signing out with the server gone still signs out', async ({ page, server }) => {
  await newAccount(page, server, 'carol');
  expect(await page.evaluate(() => localStorage.getItem('token'))).toBeTruthy();
  await server.stop();
  await expect.poll(() => page.evaluate(() => !conn.dc)).toBe(true);
  const reloaded = page.waitForRequest(server.url); // fails to load: the server is gone
  await page.locator('#settings-btn').click();
  await page.locator('#logout').click();
  await reloaded;
  await server.start(true);
  await page.goto(server.url);
  await expect(page.locator('#login')).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem('token'))).toBeNull();
});

// Signing out reloads the page, which forgets everything anyway; a
// session ended from another device does not.
test('the next user on the same tab does not see the last one\'s games', async ({ page, server, browser }) => {
  const email = await newAccount(page, server, 'dave');
  await page.locator('#bot-picks button.pick').first().click();
  await expect(page.locator('#my-games li:not(.empty-note)')).toHaveCount(1);

  const elsewhere = await (await browser.newContext()).newPage();
  await enterCode(elsewhere, await emailSignIn(elsewhere, server, 'dave', email));
  await expect(elsewhere.locator('#lobby')).toBeVisible();
  await openAccount(elsewhere);
  await elsewhere.locator('#sign-out-others').click();
  await expect(elsewhere.locator('#sign-out-msg')).toContainText('signed out');

  await expect(page.locator('#login')).toBeVisible();
  await newAccount(page, server, 'erin', true);
  await expect(page.locator('#my-games li:not(.empty-note)')).toHaveCount(0);
});

test('a game that ended while the page was offline shows as finished', async ({ page, server, browser }) => {
  const email = await newAccount(page, server, 'fay');
  await page.locator('#bot-picks button.pick').first().click();
  await expect(page.locator('#my-games li:not(.empty-note)')).toHaveCount(1);

  // Drop this page's connection and keep it from coming back.
  await page.context().setOffline(true);
  await page.evaluate(() => conn.dc.close());
  await expect.poll(() => page.evaluate(() => !conn.dc)).toBe(true);

  const elsewhere = await (await browser.newContext()).newPage();
  await enterCode(elsewhere, await emailSignIn(elsewhere, server, 'fay', email));
  await expect(elsewhere.locator('#lobby')).toBeVisible();
  // Its only game opens by itself.
  await expect(elsewhere.locator('#my-games li.selected')).toHaveCount(1);
  elsewhere.on('dialog', d => d.accept());
  await elsewhere.locator('#resign').click();
  await expect(elsewhere.locator('#recent-games li:not(.empty-note)')).toHaveCount(1);

  await page.context().setOffline(false);
  await expect(page.locator('#recent-games li:not(.empty-note)')).toHaveCount(1);
  await expect(page.locator('#my-games li:not(.empty-note)')).toHaveCount(0);
});

test('History opened twice lists each game once', async ({ page, server }) => {
  await newAccount(page, server, 'gus');
  await page.locator('#bot-picks button.pick').first().click();
  await expect(page.locator('#my-games li.selected')).toHaveCount(1);
  page.on('dialog', d => d.accept());
  await page.locator('#resign').click();
  await expect(page.locator('#recent-games li:not(.empty-note)')).toHaveCount(1);
  // Both clicks' replies arrive; only the second may fill the list.
  await page.evaluate(() => { openHistory(); openHistory(); });
  await expect(page.locator('#history-games li:not(.empty-note)')).toHaveCount(1);
  await page.waitForTimeout(500); // a late duplicate would show by now
  await expect(page.locator('#history-games li:not(.empty-note)')).toHaveCount(1);
  // A game row opens from the keyboard too.
  await page.locator('#recent-games li:not(.empty-note)').focus();
  await page.keyboard.press('Enter');
  await expect(page.locator('#history')).toBeHidden();
});
