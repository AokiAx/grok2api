const exactNumber = new Intl.NumberFormat("zh-CN", {
  maximumFractionDigits: 2,
});

const units = [
  { value: 1e15, label: "Q" },
  { value: 1e12, label: "T" },
  { value: 1e9, label: "B" },
  { value: 1e6, label: "M" },
  { value: 1e3, label: "K" },
] as const;

export type FormattedNumber = {
  display: string;
  exact: string;
};

export function formatAdaptiveNumber(value?: number): FormattedNumber {
  if (value == null || !Number.isFinite(value)) {
    return { display: "—", exact: "—" };
  }

  const exact = exactNumber.format(value);
  const unit = unitFor(Math.abs(value));
  return {
    display: unit ? `${scaledNumber(value, unit.value)}${unit.label}` : exact,
    exact,
  };
}

export function formatAdaptiveRatio(actual?: number, limit?: number) {
  const left = formatAdaptiveNumber(actual);
  const right = formatAdaptiveNumber(limit);
  const magnitude = Math.max(Math.abs(actual ?? 0), Math.abs(limit ?? 0));
  const unit = unitFor(magnitude);
  return {
    display: unit
      ? `${scaledNumber(actual ?? 0, unit.value)}/${scaledNumber(limit ?? 0, unit.value)}${unit.label}`
      : `${left.display}/${right.display}`,
    exact: `${left.exact}/${right.exact}`,
  };
}

function unitFor(value: number) {
  return units.find((unit) => value >= unit.value);
}

function scaledNumber(value: number, unit: number) {
  const scaled = value / unit;
  const digits = Math.abs(scaled) < 10 ? 2 : Math.abs(scaled) < 100 ? 1 : 0;
  const factor = 10 ** digits;
  const truncated = Math.trunc(scaled * factor) / factor;
  return new Intl.NumberFormat("zh-CN", {
    minimumFractionDigits: 0,
    maximumFractionDigits: digits,
  }).format(truncated);
}
