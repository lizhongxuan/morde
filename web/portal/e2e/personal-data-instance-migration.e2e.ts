import { expect, test, type Request } from "@playwright/test";

const serviceInstanceID = "00000000-0000-4000-8000-000000000001";
const oldServiceInstanceID = "old-service-instance-e2e";
const userID = "u1a2b3c4";
const oldDraftName = "仅保留在旧服务实例的本地草稿";

type StoredPersonalState = {
  objects: Record<string, unknown>;
  outbox: Array<Record<string, unknown>>;
  attachments: unknown[];
  cachedAttachments: unknown[];
  cursor: { initialized: boolean; afterSequence: number };
  snapshotComplete: boolean;
  syncError: string | null;
  lastSyncedAt: number | null;
};

function openPersonalDataDatabase(page: import("@playwright/test").Page) {
  return page.evaluate(async ({ oldServiceInstanceID: oldInstanceID, userID: ownerID, oldDraftName: draftName }) => {
    const database = await new Promise<IDBDatabase>((resolve, reject) => {
      const request = indexedDB.open("morde-personal-data-v1", 1);
      request.onupgradeneeded = () => {
        if (!request.result.objectStoreNames.contains("account-state")) request.result.createObjectStore("account-state");
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
    const oldScope = `${location.origin}\u0000${ownerID}\u0000${encodeURIComponent(oldInstanceID)}`;
    const state = {
      objects: {},
      outbox: [{
        id: "legacy-mutation-e2e",
        key: "legacy-idempotency-key-e2e",
        objectType: "profile",
        objectID: "default",
        baseRevision: 7,
        body: { name: draftName, avatar: "dragon", emoji: "", color: "", description: "", style: "" },
        operation: "replace",
        status: "pending",
        createdAt: Date.now(),
      }],
      attachments: [],
      cachedAttachments: [],
      cursor: { initialized: false, afterSequence: 0 },
      snapshotComplete: false,
      syncError: null,
      lastSyncedAt: null,
    };
    await new Promise<void>((resolve, reject) => {
      const transaction = database.transaction("account-state", "readwrite");
      transaction.objectStore("account-state").put(state, oldScope);
      transaction.oncomplete = () => resolve();
      transaction.onerror = () => reject(transaction.error);
      transaction.onabort = () => reject(transaction.error);
    });
    database.close();
    return { oldScope };
  }, { oldServiceInstanceID, userID, oldDraftName });
}

async function readPersonalState(page: import("@playwright/test").Page, scope: string): Promise<StoredPersonalState | undefined> {
  return page.evaluate(async (requestedScope) => {
    const database = await new Promise<IDBDatabase>((resolve, reject) => {
      const request = indexedDB.open("morde-personal-data-v1", 1);
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
    try {
      return await new Promise<StoredPersonalState | undefined>((resolve, reject) => {
        const transaction = database.transaction("account-state", "readonly");
        const request = transaction.objectStore("account-state").get(requestedScope);
        request.onsuccess = () => resolve(request.result as StoredPersonalState | undefined);
        request.onerror = () => reject(request.error);
      });
    } finally {
      database.close();
    }
  }, scope);
}

test.describe("personal data service-instance migration", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "this fixture uses only the local MSW service");

  test("keeps old-instance drafts local until the user explicitly copies them to the confirmed instance", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await expect(page.getByRole("banner").getByText("demo", { exact: true })).toBeVisible();

    const { oldScope } = await openPersonalDataDatabase(page);
    const mutationRequests: Request[] = [];
    page.on("request", (request) => {
      if (request.url().endsWith("/api/personal-data/profile/default") && request.method() === "PUT") {
        mutationRequests.push(request);
      }
    });

    await page.goto("/personal");
    const migrationSection = page.getByRole("region", { name: "其他服务实例保留的草稿" });
    await expect(migrationSection).toBeVisible();
    await migrationSection.locator("details > summary").click();
    await expect(page.getByText(oldDraftName)).toBeVisible();
    await expect(page.getByRole("button", { name: "复制到当前服务并提交" })).toBeEnabled();

    // Loading and syncing the confirmed instance must leave the old mutation untouched on the server.
    await expect.poll(() => mutationRequests.length).toBe(0);
    const quarantinedDraft = await readPersonalState(page, oldScope);
    expect(quarantinedDraft?.outbox[0]).toMatchObject({
      id: "legacy-mutation-e2e",
      key: "legacy-idempotency-key-e2e",
      status: "conflict",
      body: { name: oldDraftName },
    });

    const migrationResponse = page.waitForResponse((response) =>
      response.url().endsWith("/api/personal-data/profile/default") && response.request().method() === "PUT",
    );
    await page.getByRole("button", { name: "复制到当前服务并提交" }).click();
    const response = await migrationResponse;
    expect(response.status()).toBe(200);

    const request = response.request();
    const requestHeaders = await request.allHeaders();
    expect(requestHeaders["x-morde-service-instance-id"]).toBe(serviceInstanceID);
    expect(requestHeaders["x-morde-actor-id"]).toBe(userID);
    expect(requestHeaders["idempotency-key"]).not.toBe("legacy-idempotency-key-e2e");
    expect(request.postDataJSON()).toMatchObject({
      base_revision: 0,
      body: { name: oldDraftName, avatar: "dragon" },
    });
    expect(mutationRequests).toHaveLength(1);

    await expect.poll(async () => (await readPersonalState(page, oldScope))?.outbox[0]?.resolutionQueued).toBe(true);
    const retainedOldDraft = await readPersonalState(page, oldScope);
    expect(retainedOldDraft?.outbox[0]).toMatchObject({
      id: "legacy-mutation-e2e",
      status: "conflict",
      resolutionQueued: true,
      body: { name: oldDraftName },
    });
    const currentScope = `${new URL(page.url()).origin}\u0000${userID}\u0000${encodeURIComponent(serviceInstanceID)}`;
    await expect.poll(async () => (await readPersonalState(page, currentScope))?.outbox.length).toBe(0);
    expect((await readPersonalState(page, currentScope))?.objects['["profile","default"]']).toMatchObject({
      object_type: "profile",
      object_id: "default",
      body: { name: oldDraftName },
    });
  });
});
