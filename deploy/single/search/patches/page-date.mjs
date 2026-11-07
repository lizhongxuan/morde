import { parse } from "node-html-parser";

const MAX_JSON_LD_SCRIPTS = 16;
const MAX_JSON_LD_SCRIPT_CHARS = 64 * 1024;
const MAX_JSON_LD_NODES = 2000;
const MAX_VALUE_CHARS = 128;

const DATE_VALUE = /^(\d{4})-(\d{2})-(\d{2})(?:T(\d{2}):(\d{2})(?::(\d{2})(?:\.\d{1,9})?)?(Z|[+-]\d{2}:\d{2}))?$/;
const ARTICLE_TYPES = new Set([
  "article",
  "newsarticle",
  "blogposting",
  "liveblogposting",
  "report",
  "webpage",
]);

function normalizeDate(value) {
  if (typeof value !== "string" || value.length === 0 || value.length > MAX_VALUE_CHARS) {
    return undefined;
  }
  const candidate = value.trim();
  const match = DATE_VALUE.exec(candidate);
  if (!match) return undefined;

  const [, year, month, day, hour, minute, second, zone] = match;
  const y = Number(year);
  const m = Number(month);
  const d = Number(day);
  const calendar = new Date(0);
  calendar.setUTCFullYear(y, m - 1, d);
  if (calendar.getUTCFullYear() !== y || calendar.getUTCMonth() !== m - 1 || calendar.getUTCDate() !== d) {
    return undefined;
  }
  if (hour !== undefined) {
    if (Number(hour) > 23 || Number(minute) > 59 || (second !== undefined && Number(second) > 59)) {
      return undefined;
    }
    if (zone !== "Z") {
      const offset = /([+-])(\d{2}):(\d{2})$/.exec(zone);
      if (!offset || Number(offset[2]) > 14 || Number(offset[3]) > 59 || (Number(offset[2]) === 14 && Number(offset[3]) !== 0)) {
        return undefined;
      }
    }
  }
  return `${year}-${month}-${day}`;
}

function firstValid(values) {
  for (const value of values) {
    const date = normalizeDate(value);
    if (date) return date;
  }
  return undefined;
}

function metaValues(root, selectors) {
  const values = [];
  for (const selector of selectors) {
    for (const element of root.querySelectorAll(selector)) {
      const value = element.getAttribute("content");
      if (value !== undefined && value !== null) values.push(value);
    }
  }
  return values;
}

function jsonLdDates(root) {
  const result = { published: [], modified: [] };
  let remainingNodes = MAX_JSON_LD_NODES;
  const scripts = root.querySelectorAll('script[type="application/ld+json"]').slice(0, MAX_JSON_LD_SCRIPTS);

  for (const script of scripts) {
    const text = script.textContent ?? script.text;
    if (typeof text !== "string" || text.length === 0 || text.length > MAX_JSON_LD_SCRIPT_CHARS) continue;
    let data;
    try {
      data = JSON.parse(text);
    } catch {
      continue;
    }

    const stack = [data];
    while (stack.length && remainingNodes > 0) {
      remainingNodes -= 1;
      const value = stack.pop();
      if (Array.isArray(value)) {
        for (let i = Math.min(value.length, 100) - 1; i >= 0; i -= 1) stack.push(value[i]);
        continue;
      }
      if (!value || typeof value !== "object") continue;

      const rawTypes = value["@type"];
      const types = (Array.isArray(rawTypes) ? rawTypes : [rawTypes])
        .filter((type) => typeof type === "string")
        .map((type) => type.split(/[\/#]/u).pop().toLowerCase());
      if (types.some((type) => ARTICLE_TYPES.has(type))) {
        if (typeof value.datePublished === "string") result.published.push(value.datePublished);
        if (typeof value.dateModified === "string") result.modified.push(value.dateModified);
      }
      for (const child of Object.values(value)) {
        if (child && typeof child === "object") stack.push(child);
      }
    }
    if (remainingNodes <= 0) break;
  }
  return result;
}

/** Extract date candidates only; webpage metadata is untrusted and is not proof of recency. */
export function extractPageDateMetadata(html) {
  if (typeof html !== "string" || html.length === 0) return "";
  let root;
  try {
    root = parse(html);
  } catch {
    return "";
  }

  const publishedValues = metaValues(root, [
    'meta[property="article:published_time"]',
    'meta[property="og:article:published_time"]',
    'meta[name="date"]',
    'meta[name="pubdate"]',
    'meta[name="publishdate"]',
    'meta[name="datepublished"]',
    'meta[name="parsely-pub-date"]',
    'meta[itemprop="datePublished"]',
  ]);
  const modifiedValues = metaValues(root, [
    'meta[property="article:modified_time"]',
    'meta[property="og:article:modified_time"]',
    'meta[name="datemodified"]',
    'meta[itemprop="dateModified"]',
  ]);

  for (const element of root.querySelectorAll('time[itemprop="datePublished"][datetime]')) {
    publishedValues.push(element.getAttribute("datetime"));
  }
  for (const element of root.querySelectorAll('time[itemprop="dateModified"][datetime]')) {
    modifiedValues.push(element.getAttribute("datetime"));
  }

  const structured = jsonLdDates(root);
  const published = firstValid([...publishedValues, ...structured.published]);
  const modified = firstValid([...modifiedValues, ...structured.modified]);
  if (!published && !modified) return "";

  const lines = ["## Page date metadata (unverified)"];
  if (published) lines.push(`- Published: ${published}`);
  if (modified) lines.push(`- Modified: ${modified}`);
  return lines.join("\n");
}
