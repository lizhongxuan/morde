/**
 * The curated Ideas catalogue (contract C5), sourced from the shared repository data and
 * shipped by the native app and desktop harness byte for byte.
 */
import type { Idea } from "../types";
import en from "../../../shared/ideas/ideas.en.json";
import zh from "../../../shared/ideas/ideas.zh.json";

export interface CatalogueIdea {
  id: string;
  emoji: string;
  title: string;
  body: string;
  kind: "CHAT" | "ROUTINE" | "GOAL" | string;
  time?: string;
  category?: string;
  prompt: string;
}

export interface CatalogueSection {
  id: string;
  title: string;
  ideas: CatalogueIdea[];
}

export interface Catalogue {
  sections: CatalogueSection[];
}

/** The catalogue in the app's language: Chinese for any `zh` locale, English otherwise. */
export function catalogue(locale: string): Catalogue {
  return (locale.toLowerCase().startsWith("zh") ? zh : en) as Catalogue;
}

/** A catalogue idea in the shape the page renders (the runtime's `Idea`), with its own emoji. */
export function asIdea(c: CatalogueIdea): Idea & { emoji: string; id: string } {
  return {
    id: c.id,
    emoji: c.emoji,
    title: c.title,
    detail: c.body,
    prompt: c.prompt,
    kind: c.kind.toLowerCase(),
    time: c.time,
    category: c.category,
  };
}
