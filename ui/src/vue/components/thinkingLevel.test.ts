import {
  CONCRETE_THINKING_LEVELS,
  defaultThinkingLevelForModel,
  normalizeThinkingLevelForModel,
  roundThinkingLevel,
  supportedThinkingLevels,
} from "./thinkingLevel";

let passed = 0;
let failed = 0;

function expectRound(
  level: Parameters<typeof roundThinkingLevel>[0],
  supported: Parameters<typeof roundThinkingLevel>[1],
  want: ReturnType<typeof roundThinkingLevel>,
) {
  const got = roundThinkingLevel(level, supported);
  if (got === want) {
    passed++;
  } else {
    failed++;
    console.error(
      `FAIL: roundThinkingLevel(${level}, ${supported.join(",")}) = ${got}, want ${want}`,
    );
  }
}

expectRound("high", ["low", "high"], "high");
expectRound("max", ["off", "high", "xhigh"], "xhigh");
expectRound("xhigh", ["high", "max"], "high");
expectRound("minimal", ["off", "low"], "low");
expectRound("off", ["low", "high"], "low");

function expectModelLevel(
  level: Parameters<typeof normalizeThinkingLevelForModel>[0],
  model: Parameters<typeof normalizeThinkingLevelForModel>[1],
  want: ReturnType<typeof normalizeThinkingLevelForModel>,
) {
  const got = normalizeThinkingLevelForModel(level, model);
  if (got === want) {
    passed++;
  } else {
    failed++;
    console.error(`FAIL: normalizeThinkingLevelForModel(${level}) = ${got}, want ${want}`);
  }
}

expectModelLevel(
  "max",
  { supports_reasoning: true, reasoning_levels: ["off", "high", "xhigh"] },
  "xhigh",
);
expectModelLevel("minimal", { supports_reasoning: true, reasoning_levels: ["off", "low"] }, "low");
expectModelLevel("high", { supports_reasoning: true, reasoning_levels: ["off"] }, "default");
expectModelLevel("high", { supports_reasoning: false }, "default");
expectModelLevel("max", { supports_reasoning: true }, "default");
for (const level of CONCRETE_THINKING_LEVELS) {
  expectModelLevel(level, undefined, "default");
  for (const model of [{}, { supports_reasoning: true }, { reasoning_levels: [] }]) {
    expectModelLevel(level, model, level === "max" ? "default" : level);
  }
}
expectModelLevel("default", { reasoning_levels: ["high"] }, "default");
expectModelLevel("max", { reasoning_levels: ["low", "max"] }, "max");
expectModelLevel("high", { supports_reasoning: false, reasoning_levels: ["high"] }, "default");

function expectSupported(
  model: Parameters<typeof supportedThinkingLevels>[0],
  want: readonly string[],
) {
  const got = supportedThinkingLevels(model);
  if (got.join(",") === want.join(",")) {
    passed++;
  } else {
    failed++;
    console.error(`FAIL: supportedThinkingLevels = ${got.join(",")}, want ${want.join(",")}`);
  }
}

expectSupported({ supports_reasoning: true, reasoning_levels: ["off", "high", "max"] }, [
  "off",
  "high",
  "max",
]);
expectSupported({ supports_reasoning: false }, []);
for (const model of [undefined, {}, { supports_reasoning: true }, { reasoning_levels: [] }]) {
  expectSupported(model, ["off", "minimal", "low", "medium", "high", "xhigh"]);
}
expectSupported({ reasoning_levels: ["high", "low"] }, ["high", "low"]);
expectSupported({ supports_reasoning: false, reasoning_levels: ["high"] }, []);
expectSupported({ reasoning_levels: ["low"], default_reasoning_level: "high" }, ["low"]);

function expectDefault(
  model: Parameters<typeof defaultThinkingLevelForModel>[0],
  want: ReturnType<typeof defaultThinkingLevelForModel>,
) {
  const got = defaultThinkingLevelForModel(model);
  if (got === want) {
    passed++;
  } else {
    failed++;
    console.error(
      `FAIL: defaultThinkingLevelForModel(${JSON.stringify(model)}) = ${got}, want ${want}`,
    );
  }
}

expectDefault(undefined, "default");
expectDefault({ supports_reasoning: true, default_reasoning_level: "high" }, "high");
expectDefault({ reasoning_levels: [], default_reasoning_level: "high" }, "high");
expectDefault({ default_reasoning_level: "high" }, "high");
expectDefault({ default_reasoning_level: "max" }, "default");
expectDefault({ reasoning_levels: ["low", "max"], default_reasoning_level: "high" }, "default");
expectDefault({ reasoning_levels: ["high"], default_reasoning_level: "default" }, "default");
expectDefault({ reasoning_levels: ["high"], default_reasoning_level: "dynamic" }, "default");
expectDefault({ reasoning_levels: ["high"], default_reasoning_level: "high" }, "high");
expectDefault({ reasoning_levels: ["off"], default_reasoning_level: "off" }, "off");
expectDefault(
  { supports_reasoning: false, reasoning_levels: ["high"], default_reasoning_level: "high" },
  "default",
);

if (failed > 0) process.exit(1);
console.log(`thinkingLevel: ${passed} passed`);
