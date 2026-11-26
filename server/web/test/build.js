// Builds cribserver once for all tests.
const { execFileSync } = require('child_process');
const path = require('path');

module.exports = () => {
  execFileSync('go', ['build', '-o', path.join(__dirname, 'cribserver.test-bin'), './cmd/cribserver'],
    { cwd: path.join(__dirname, '../..'), stdio: 'inherit' });
};
