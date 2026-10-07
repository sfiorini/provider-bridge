import { screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import { renderWithConsoleProviders } from "../../test/renderWithConsoleProviders";
import * as configGraph from "../../rpc/configGraph";
import { configGraphFixture } from "../../test/configGraphFixtures";
import { SecurityPage } from "./SecurityPage";

describe("SecurityPage", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  test("renders server security fields with write-only auth token", async () => {
    vi.spyOn(configGraph, "getConfigGraph").mockResolvedValue(configGraphFixture());

    renderWithConsoleProviders(<SecurityPage />);

    expect(await screen.findByRole("heading", { level: 2, name: "Server" })).toBeInTheDocument();
    expect(within(screen.getByLabelText("Server")).getByRole("heading", { level: 3, name: "main" })).toBeInTheDocument();
    expect(within(screen.getByLabelText("Server main status")).getByText("Restart required")).toBeInTheDocument();
    expect(within(screen.getByLabelText("Server main status")).getByText("Critical")).toBeInTheDocument();
    expect(screen.getByLabelText("Listen address")).toHaveValue(":38440");
    expect(screen.getByLabelText("Max sessions")).toHaveValue("64");
    expect(screen.getByLabelText("Session TTL")).toHaveValue("24h");
    expect(screen.getByLabelText("Auth token")).toHaveValue("");
    expect(screen.queryByDisplayValue("******")).not.toBeInTheDocument();
    expect(screen.getByText("Restart required")).toBeInTheDocument();
    expect(getMaterialTextField(document, "Auth token").supportingText).toBe(
      "Enter a new value to replace the saved secret."
    );
  });
});

type MaterialTextFieldElement = HTMLElement & {
  label: string;
  supportingText: string;
};

function getMaterialTextField(container: ParentNode, label: string) {
  const element = Array.from(container.querySelectorAll<MaterialTextFieldElement>("md-outlined-text-field")).find(
    (candidate) => candidate.label === label || candidate.getAttribute("aria-label") === label
  );
  if (!element) {
    throw new Error(`Expected a Material Web outlined text field labelled "${label}".`);
  }
  return element;
}
