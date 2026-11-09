ARG NODE_IMAGE=node:24-slim@sha256:a5f4366e834695d5f363b920c104953a535d3a9864d1bdf74566a0ebe01daee5
ARG RUNTIME_IMAGE=debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

FROM ${NODE_IMAGE} AS build

WORKDIR /opt/morde-cloud-runtime
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund
COPY bin.mjs ./bin.mjs
COPY bin-account.mjs ./bin-account.mjs
COPY src ./src
COPY test ./test
RUN npm test

FROM ${RUNTIME_IMAGE} AS runtime

WORKDIR /opt/morde-cloud-runtime
COPY --from=build /usr/local/bin/node /usr/local/bin/node
COPY --from=build --chown=65532:65532 /opt/morde-cloud-runtime/package.json ./package.json
COPY --from=build --chown=65532:65532 /opt/morde-cloud-runtime/package-lock.json ./package-lock.json
COPY --from=build --chown=65532:65532 /opt/morde-cloud-runtime/node_modules ./node_modules
COPY --from=build --chown=65532:65532 /opt/morde-cloud-runtime/bin.mjs ./bin.mjs
COPY --from=build --chown=65532:65532 /opt/morde-cloud-runtime/bin-account.mjs ./bin-account.mjs
COPY --from=build --chown=65532:65532 /opt/morde-cloud-runtime/src ./src
COPY --chown=65532:65532 smoke-account-runtime.mjs ./smoke-account-runtime.mjs

USER 65532:65532
