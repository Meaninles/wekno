const mutedMethods = [
  "log",
  "info",
  "debug",
  "trace",
  "table",
  "group",
  "groupCollapsed",
  "groupEnd",
  "time",
  "timeLog",
  "timeEnd",
  "count",
  "countReset",
  "dir",
  "dirxml",
] as const;

const noop = () => undefined;

if (typeof window !== "undefined" && typeof console !== "undefined") {
  for (const method of mutedMethods) {
    if (typeof console[method] === "function") {
      Object.defineProperty(console, method, {
        value: noop,
        configurable: true,
        writable: true,
      });
    }
  }
}
