import { createContext, type ReactNode, useContext, useMemo } from "react";
import { type Locale, type MessageKey, messages, normalizeLocale } from "./messages";

export const CONSOLE_LOCALE_STORAGE_KEY = "providerbridge.console.locale";

type InterpolationValue = string | number;

type I18nContextValue = {
  locale: Locale;
  t: (key: MessageKey, values?: Record<string, InterpolationValue>) => string;
};

const I18nContext = createContext<I18nContextValue | undefined>(undefined);

export function I18nProvider({ children }: { children: ReactNode }) {
  const value = useMemo<I18nContextValue>(() => {
    const locale = readInitialLocale();
    return {
      locale,
      t: (key, values) => translateMessageForLocale(locale, key, values)
    };
  }, []);

  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>;
}

export function useI18n() {
  const context = useContext(I18nContext);
  if (!context) {
    throw new Error("useI18n must be used within I18nProvider");
  }
  return context;
}

export function translateMessage(key: MessageKey, values?: Record<string, InterpolationValue>) {
  return translateMessageForLocale(readInitialLocale(), key, values);
}

function translateMessageForLocale(
  locale: Locale,
  key: MessageKey,
  values?: Record<string, InterpolationValue>
) {
  return interpolate(messages[locale][key], values);
}

function readInitialLocale(): Locale {
  // The console is English-only. The storage key survives for future locales,
  // but any persisted non-English value is ignored.
  return normalizeLocale(safeGetStorage(CONSOLE_LOCALE_STORAGE_KEY) ?? undefined);
}

function interpolate(message: string, values?: Record<string, InterpolationValue>) {
  if (!values) {
    return message;
  }
  return Object.entries(values).reduce(
    (result, [key, value]) => result.replaceAll(`{${key}}`, String(value)),
    message
  );
}

function safeGetStorage(key: string): string | null {
  try {
    return window.localStorage?.getItem(key) ?? null;
  } catch {
    return null;
  }
}
