// Issue #124 owns this manually invoked SDK fixture. Retain it with the
// bootstrap configuration; review it when the pinned release tooling changes.
'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

let networkAttempts = 0;
const denyNetwork = () => {
  networkAttempts++;
  throw new Error('Network requests are forbidden in the bootstrap fixture');
};
// Install guards before loading the SDK; every GitHub read below is synthetic.
for (const name of ['http', 'https', 'net', 'tls', 'http2']) {
  const transport = require('node:' + name);
  for (const method of ['request', 'get', 'connect', 'createConnection']) {
    if (typeof transport[method] === 'function') transport[method] = denyNetwork;
  }
}
require('node:net').Socket.prototype.connect = denyNetwork;
globalThis.fetch = denyNetwork;

const sdk = require(process.argv[2] ? path.resolve(process.argv[2]) : 'release-please');
assert.equal(sdk.VERSION, '17.3.0', 'use the pinned release-please SDK');
const root = path.resolve(__dirname, '../..');
const readJSON = file => JSON.parse(fs.readFileSync(path.join(root, file), 'utf8'));
const config = readJSON('release-please-config.json');
const versions = readJSON('.release-please-manifest.json');
assert.equal(config['release-type'], 'go');
for (const [key, value] of Object.entries({
  draft: true, 'bump-minor-pre-major': true, 'bump-patch-for-minor-pre-major': false,
  'include-v-in-tag': true, 'include-component-in-tag': false,
})) assert.equal(config[key], value, key);
for (const [types, hidden] of [
  [['feat', 'fix', 'perf', 'revert'], false],
  [['docs', 'test', 'ci', 'chore', 'refactor', 'style', 'build'], true],
]) {
  for (const type of types) {
    const section = config['changelog-sections'].find(entry => entry.type === type);
    assert.ok(section, 'missing changelog section: ' + type);
    assert.equal(section.hidden ?? false, hidden, type);
  }
}

let reads = 0;
let unexpectedCalls = 0;
function readOnlyGitHub(sourceConfig, sourceVersions) {
  return new Proxy({
    repository: { owner: 'fixture', repo: 'fixture', defaultBranch: 'main' },
    async getFileJson(file, branch) {
      assert.equal(branch, 'main');
      reads++;
      if (file === 'release-please-config.json') return structuredClone(sourceConfig);
      if (file === '.release-please-manifest.json') return structuredClone(sourceVersions);
      throw new Error('Unexpected fixture file: ' + file);
    },
    async *releaseIterator() { reads++; },
    async *tagIterator() { reads++; },
    async *mergeCommitIterator(branch) {
      assert.equal(branch, 'main');
      reads++;
      yield { sha: 'a'.repeat(40), message: 'feat: add a fixture behavior', files: ['cmd/main.go'] };
    },
  }, {
    get(target, property) {
      if (Object.hasOwn(target, property)) return target[property];
      return () => {
        unexpectedCalls++;
        throw new Error('Unexpected GitHub API; writes are forbidden: ' + String(property));
      };
    },
  });
}

async function check(name, sourceConfig, sourceVersions, expected) {
  const before = JSON.stringify([sourceConfig, sourceVersions]);
  const manifest = await sdk.Manifest.fromManifest(readOnlyGitHub(sourceConfig, sourceVersions), 'main');
  const proposed = await manifest.buildPullRequests();
  assert.equal(proposed.length, 1, name);
  const version = proposed[0].version.toString();
  assert.equal(version, expected, name);
  assert.equal(JSON.stringify([sourceConfig, sourceVersions]), before, 'fixture inputs changed');
  return { case: name, proposedVersion: version,
    updatedPaths: proposed[0].updates.map(update => update.path) };
}

(async () => {
  const checkedIn = await check('checked-in first release', config, versions, '0.1.0');
  assert.deepEqual(versions, { '.': '0.0.0' }, 'the manifest must not claim a prior 0.1.0 release');
  assert.equal(config['initial-version'], '0.1.0');
  const withoutInitial = structuredClone(config);
  delete withoutInitial['initial-version'];
  const defaults = await check('missing initial version control', withoutInitial, versions, '1.0.0');
  const priorVersion = await check('nonzero prior-version control', config, { '.': '0.1.0' }, '0.2.0');
  assert.equal(unexpectedCalls, 0);
  assert.equal(networkAttempts, 0);
  console.log(JSON.stringify({ sdkVersion: sdk.VERSION,
    evidence: 'actual SDK with synthetic reads; no GitHub PR, tag or release created',
    reads, unexpectedCalls, networkAttempts, cases: [checkedIn, defaults, priorVersion] }, null, 2));
})().catch(error => { console.error(error); process.exitCode = 1; });
