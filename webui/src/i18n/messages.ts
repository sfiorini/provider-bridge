import { en } from "./locales/en";

// The console is English-only. Keep the Locale/messages plumbing so additional
// locales can be reintroduced without touching every useI18n consumer.
export type Locale = "en-US";
export type MessageKey = keyof typeof en;
export type Messages = Record<MessageKey, string>;

export const messages: Record<Locale, Messages> = {
  "en-US": en
};

export function normalizeLocale(_value: string | undefined): Locale {
  return "en-US";
}
