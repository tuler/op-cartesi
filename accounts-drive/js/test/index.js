// Entry point so `node --test <this directory>` works on Node versions that
// treat a directory argument as a literal script path: importing the test
// modules registers every test with node:test.

import './keccak.test.js';
import './drive.test.js';
import './store.test.js';
import './golden.test.js';
