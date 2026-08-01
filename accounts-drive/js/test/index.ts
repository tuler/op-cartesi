// Entry point so `node --test test/` works: Node resolves the directory
// argument as a script through test/package.json's "main", and importing
// the test modules registers every test with node:test.

import './keccak.test.ts';
import './drive.test.ts';
import './store.test.ts';
import './golden.test.ts';
