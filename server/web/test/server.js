// A test fixture: a cribserver of its own for each test, on a free port,
// with an empty database and emails written to its log. WEB_DIR serves
// another copy of the web client (e.g. an older one, to check that a test
// fails there).
const base = require('@playwright/test');
const { spawn } = require('child_process');
const fs = require('fs');
const net = require('net');
const os = require('os');
const path = require('path');

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.on('error', reject);
    s.listen(0, '127.0.0.1', () => { const { port } = s.address(); s.close(() => resolve(port)); });
  });
}

class Server {
  // start starts the server; again restarts it on the same port and data.
  async start(again) {
    if (!again) {
      this.dir = fs.mkdtempSync(path.join(os.tmpdir(), 'crib-web-test-'));
      this.port = await freePort();
    }
    this.url = `http://127.0.0.1:${this.port}/`;
    this.log = '';
    const args = ['-http', `127.0.0.1:${this.port}`, '-tcp', '127.0.0.1:0', '-rtc-port', '0',
      '-db', path.join(this.dir, 'test.db'), '-stats', '0'];
    if (process.env.WEB_DIR) args.push('-web-dir', process.env.WEB_DIR);
    this.proc = spawn(path.join(__dirname, 'cribserver.test-bin'), args, { stdio: ['ignore', 'pipe', 'pipe'] });
    this.proc.stdout.on('data', d => { this.log += d; });
    this.proc.stderr.on('data', d => { this.log += d; });
    for (let i = 0; i < 100; i++) {
      try { if ((await fetch(this.url + 'api/lobby')).ok) return; } catch (e) { /* not up yet */ }
      await new Promise(r => setTimeout(r, 100));
    }
    throw new Error('cribserver did not start:\n' + this.log);
  }

  // stop kills the server; the test's own data goes with it.
  async stop() {
    if (this.proc.exitCode === null) {
      const exited = new Promise(r => this.proc.once('exit', r));
      this.proc.kill();
      await exited;
    }
  }

  // mails returns the bodies of the emails sent to the address so far.
  mails(to) {
    return this.log.split('\n')
      .filter(l => l.includes('msg="mail (not sent: no -smtp)"') && l.includes(`to=${to} `))
      .map(l => JSON.parse(/ body=("(?:[^"\\]|\\.)*")/.exec(l)[1]));
  }

  // lastMail waits for an email to the address beyond the first `after`.
  async lastMail(to, after = 0) {
    for (let i = 0; i < 100; i++) {
      const m = this.mails(to);
      if (m.length > after) return m[m.length - 1];
      await new Promise(r => setTimeout(r, 100));
    }
    throw new Error(`no email to ${to}:\n${this.log}`);
  }
}

exports.test = base.test.extend({
  server: async ({}, use) => {
    const s = new Server();
    await s.start();
    await use(s);
    await s.stop();
    fs.rmSync(s.dir, { recursive: true, force: true });
  },
});
exports.expect = base.expect;
