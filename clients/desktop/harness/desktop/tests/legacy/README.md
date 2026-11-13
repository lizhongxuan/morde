# Unshipped shell tests

These tests target `src/main.ts` and `src/operator.ts`. The desktop package ships `out/morde-main.js`; neither legacy source is included by `tsconfig.json` or `tsconfig.tests.json`, and `@computer-use/nut-js` is not a package dependency. They stay here for reference and are not part of `npm test`. Active window, Portal, recovery, and shutdown behavior is covered by the `morde-main-*` tests.
