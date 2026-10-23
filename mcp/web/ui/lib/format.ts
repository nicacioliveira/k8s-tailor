// Number formatting (en-US). Always Intl — never "$" + toFixed by hand.
// Negative numbers use the typographic minus (U+2212): same width as "+", aligns in
// tabular columns and never reads as a hyphen.
const minus = (s: string) => s.replace(/^-/, "−");

const num = new Intl.NumberFormat("en-US");
const usd = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD" });
const pct = new Intl.NumberFormat("en-US", {
  style: "percent",
  minimumFractionDigits: 1,
  maximumFractionDigits: 1,
  signDisplay: "exceptZero",
});

/** 1234567.8 -> "1,234,567.8". `digits` fixes the decimals. */
export function formatNumber(value: number, digits?: number): string {
  return minus(
    digits === undefined
      ? num.format(value)
      : new Intl.NumberFormat("en-US", {
          minimumFractionDigits: digits,
          maximumFractionDigits: digits,
        }).format(value),
  );
}

/** 12.5 -> "$12.50"; -3 -> "−$3.00". */
export function formatUSD(value: number): string {
  return minus(usd.format(value));
}

/** Percentage points in: 12.3 -> "+12.3%"; -5.2 -> "−5.2%". */
export function formatDelta(percent: number): string {
  return minus(pct.format(percent / 100));
}
