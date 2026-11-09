import { describe, expect, it } from "vitest";
import { tokenFromLink } from "./api";
import { BUCKET_S, expiry, hmacSha256, sha256, sign, signedParams } from "./ticket";

const enc = new TextEncoder();
const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");

describe("ticket", () => {
  it("computes SHA-256 (FIPS vectors, block boundaries, non-ASCII)", () => {
    const known: Array<[string, string]> = [
      ["", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"],
      ["abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"],
      ["a".repeat(55), "9f4390f8d30c2dd92ec9f095b65e2b9ae9b0a925a5258e241c9f1e910f734318"],
      ["a".repeat(56), "b35439a4ac6f0948b6d6f9e3c6af0f5f590ce20f1bde7090ef7970686ec6738a"],
      ["a".repeat(64), "ffe054fe7ae0cb6dc65c3af9b61d5209f439851db43d0ba5997337df154668eb"],
      ["a".repeat(1000), "41edece42d63e8d9bf515a9ba6932e1c20cbc9f5a5d134645adb5db1b9737ea3"],
      ["中文 文件.webp", "dbd806ea14642abd03cc196f14d07bc73d2a2e36ccdf88afaf0ee44138ca6f9a"],
    ];
    for (const [text, digest] of known) expect(hex(sha256(enc.encode(text)))).toBe(digest);
  });

  it("computes HMAC-SHA256 (RFC 4231 cases 1, 2, 6 and 7; a long key of our own)", () => {
    const aa = new Uint8Array(131).fill(0xaa);
    expect(hex(hmacSha256(new Uint8Array(20).fill(0x0b), enc.encode("Hi There")))).toBe(
      "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7",
    );
    expect(hex(hmacSha256(enc.encode("Jefe"), enc.encode("what do ya want for nothing?")))).toBe(
      "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843",
    );
    expect(hex(hmacSha256(aa, enc.encode("Test Using Larger Than Block-Size Key - Hash Key First")))).toBe(
      "60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54",
    );
    expect(
      hex(
        hmacSha256(
          aa,
          enc.encode(
            "This is a test using a larger than block-size key and a larger than block-size data. The key needs to be hashed before being used by the HMAC algorithm.",
          ),
        ),
      ),
    ).toBe("9b09ffa71b942fcb27635fbcd5b0e944bfdc63644f0713938a7f51535c3a35e2");
    expect(hex(hmacSha256(enc.encode("y".repeat(200)), enc.encode("hello")))).toBe(
      "ea59ac9d323b101a051980745828bcceb5dc636f6f54caff2538d5c1a3f6c915",
    );
  });

  it("signs a path the way the server checks it (tickets.py): exp, newline, path; 32 hex chars", () => {
    // hmac.new(b"secret-token", "1700006400\n/api/files/avatar/猫/still.webp".encode(), sha256).hexdigest()[:32]
    expect(sign("secret-token", "/api/files/avatar/猫/still.webp", 1_700_006_400)).toBe("059bb504d9787e90e8f3e288b5d2cafd");
  });

  it("picks the second six-hour boundary ahead, so a link's text holds for hours", () => {
    const now = 1_700_000_000_000;
    const exp = expiry(now);
    expect(exp % BUCKET_S).toBe(0);
    expect(exp - now / 1000).toBeGreaterThan(BUCKET_S);
    expect(exp - now / 1000).toBeLessThanOrEqual(2 * BUCKET_S);
    expect(expiry(now + 60_000)).toBe(exp);
    const params = signedParams("t", "/api/files/a.png", now);
    expect(params.get("exp")).toBe(String(exp));
    expect(params.get("sig")).toBe(sign("t", "/api/files/a.png", exp));
  });
});

describe("tokenFromLink", () => {
  it("reads the token off the fragment, leaving the rest of the hash", () => {
    const url = new URL("http://192.168.1.20:8787/#token=abc%2Fd");
    expect(tokenFromLink(url)).toBe("abc/d");
    expect(url.hash).toBe("");
    const two = new URL("http://127.0.0.1:8787/#devices&token=xyz");
    expect(tokenFromLink(two)).toBe("xyz");
    expect(two.hash).toBe("#devices");
  });

  it("still takes the older ?token= form, and nothing from a plain address", () => {
    const old = new URL("http://127.0.0.1:8787/?token=old&x=1");
    expect(tokenFromLink(old)).toBe("old");
    expect(old.search).toBe("?x=1");
    expect(tokenFromLink(new URL("http://127.0.0.1:8787/#devices"))).toBeNull();
  });
});
