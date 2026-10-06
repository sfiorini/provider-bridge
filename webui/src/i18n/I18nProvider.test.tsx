import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import {
  CONSOLE_LOCALE_STORAGE_KEY,
  I18nProvider,
  translateMessage,
  useI18n
} from "./I18nProvider";

function Probe() {
  const { locale, t } = useI18n();
  return (
    <div>
      <p data-testid="locale">{locale}</p>
      <p data-testid="title">{t("nav.overview")}</p>
      <p data-testid="routes">{t("overview.routes")}</p>
    </div>
  );
}

describe("I18nProvider", () => {
  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  test("defaults to English regardless of navigator language", () => {
    vi.spyOn(window.navigator, "language", "get").mockReturnValue("zh-CN");

    render(
      <I18nProvider>
        <Probe />
      </I18nProvider>
    );

    expect(screen.getByTestId("locale")).toHaveTextContent("en-US");
    expect(screen.getByTestId("title")).toHaveTextContent("Overview");
    expect(screen.getByTestId("routes")).toHaveTextContent("Routes");
  });

  test("ignores a persisted non-English locale", () => {
    localStorage.setItem(CONSOLE_LOCALE_STORAGE_KEY, "zh-CN");

    render(
      <I18nProvider>
        <Probe />
      </I18nProvider>
    );

    expect(screen.getByTestId("locale")).toHaveTextContent("en-US");
    expect(screen.getByTestId("title")).toHaveTextContent("Overview");
  });

  test("translates messages outside React in English", () => {
    localStorage.setItem(CONSOLE_LOCALE_STORAGE_KEY, "zh-CN");

    expect(translateMessage("error.requestFailedWithStatus", { status: 502 })).toBe(
      "Request failed with status 502"
    );
  });

  test("keeps working when localStorage is unavailable", () => {
    const original = Object.getOwnPropertyDescriptor(window, "localStorage");
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      get() {
        throw new DOMException("blocked", "SecurityError");
      }
    });
    vi.spyOn(window.navigator, "language", "get").mockReturnValue("en-US");

    render(
      <I18nProvider>
        <Probe />
      </I18nProvider>
    );

    expect(screen.getByTestId("locale")).toHaveTextContent("en-US");
    expect(screen.getByTestId("title")).toHaveTextContent("Overview");

    if (original) {
      Object.defineProperty(window, "localStorage", original);
    }
  });
});
