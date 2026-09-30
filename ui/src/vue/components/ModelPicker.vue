<!-- Unified model + reasoning-effort picker, built on PrimeVue <Select>.
     One quiet control replaces the previous pair of labeled dropdowns: the
     trigger reads "Claude Opus 4.8 · medium" and opens a single popover with
     search, the model list, an inline reasoning-effort pill row, and a
     "Manage models…" / refresh footer.

     Design notes for the default (non-customized) install:
     - Model ids are humanized via prettyModelLabels (claude-opus-4.8 ->
       "Claude Opus 4.8"); unknown ids pass through verbatim and label
       collisions fall back to raw ids.
     - Source sub-labels only render when the catalog actually spans more
       than one source — a single-gateway install never repeats its hostname
       under every option.
     - Tier-2 models stay behind a "All models (N)" toggle; an active search
       spans all models so nothing is unfindable.
     PrimeVue owns open/close, outside-click, Escape and viewport-aware flip
     placement; styling uses size="small" + the shared statusPickerDt token
     map.

     The overlay is appended to the trigger for the boxed variant and to <body>
     for the inline one. append-to="self" positions with relativePosition(),
     which aligns left edges and never clamps to the viewport — fine for the
     composer's full-width trigger, but the inline trigger sits at the right edge
     of the status bar, from where the panel hung off the left of the screen on a
     narrow viewport. The body portal takes absolutePosition(), which does clamp
     and flip. -->
<template>
  <Select
    ref="selectRef"
    :model-value="selectedPickerValue"
    :options="optionGroups"
    option-label="label"
    option-value="pickerValue"
    option-group-label="label"
    option-group-children="items"
    :option-disabled="(m: PickerOption) => !m.ready || m.id === disabledModel"
    :disabled="disabled"
    fluid
    size="small"
    :dt="statusPickerDt"
    :scroll-height="scrollHeight"
    :class="['model-picker', { 'model-picker-inline': inline }]"
    :aria-label="ariaLabel"
    :append-to="inline || appendToBody ? 'body' : 'self'"
    :pt="{
      overlay: { class: ['model-picker-panel', panelClass] },
      label: disabledReason ? { 'aria-description': disabledReason } : {},
    }"
    filter
    :filter-fields="['label', 'id', 'source']"
    :filter-placeholder="t('searchModels')"
    :empty-filter-message="t('noModelsFound')"
    reset-filter-on-hide
    :auto-filter-focus="autoFilterFocus"
    :focus-on-hover="false"
    @update:model-value="handleSelect"
    @filter="onFilter"
    @hide="filterValue = ''"
  >
    <template #value>
      <span class="model-picker-value">
        <span :class="['model-picker-value-name', { 'status-readout-affordance': inline }]">{{
          triggerLabel || selectedLabel
        }}</span>
        <span v-if="effortText && !inline && !triggerLabel" class="model-picker-value-effort"
          >· {{ effortText }}</span
        >
      </span>
    </template>
    <template #optiongroup="{ option }">
      <span v-if="option.kind === 'recent'" class="model-picker-group-label">{{
        t("recentModels")
      }}</span>
      <span v-else class="model-picker-group-divider" aria-hidden="true" />
    </template>
    <template #option="{ option }">
      <div
        :class="[
          'model-picker-option-content',
          { 'model-picker-option-recent': option.kind === 'recent' },
        ]"
      >
        <span class="model-picker-option-name">{{ option.label }}</span>
        <span
          v-if="option.source && option.source !== dominantSource"
          class="model-picker-option-source"
          >{{ option.source }}</span
        >
      </div>
      <span
        v-if="option.kind === 'recent' && option.recentEffortLabel"
        class="model-picker-option-effort"
      >
        {{ option.recentEffortLabel }}
      </span>
      <span v-if="!option.ready" class="model-picker-option-badge">{{ t("notReadyBadge") }}</span>
      <svg
        v-else-if="option.kind === 'model' && option.id === selectedModel"
        class="model-picker-option-check"
        width="14"
        height="14"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        stroke-width="2.5"
        stroke-linecap="round"
        stroke-linejoin="round"
        aria-hidden="true"
      >
        <path d="M20 6L9 17l-5-5" />
      </svg>
    </template>
    <template #footer>
      <button
        v-if="tier2Models.length > 0 && !filterValue"
        class="model-picker-more"
        type="button"
        @click="toggleMore"
      >
        <svg
          :class="`model-picker-more-icon ${showMore ? 'expanded' : ''}`"
          width="12"
          height="12"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
        >
          <path d="M6 9l6 6 6-6" />
        </svg>
        <span>{{
          showMore ? t("showFewerModels") : `${t("showAllModels")} (${models.length})`
        }}</span>
      </button>
      <template v-if="showReasoning && reasoningSupported">
        <div class="model-picker-divider" />
        <div class="model-picker-effort">
          <span :id="effortLabelId" class="model-picker-effort-label">{{ t("effortLabel") }}</span>
          <div class="model-picker-effort-pills" role="radiogroup" :aria-labelledby="effortLabelId">
            <button
              v-for="level in effortLevels"
              :key="level.value"
              type="button"
              role="radio"
              :aria-checked="level.value === effectiveEffort"
              :class="`model-picker-effort-pill${level.value === effectiveEffort ? ' active' : ''}`"
              @click="selectEffort(level.value)"
            >
              {{ level.label }}
            </button>
          </div>
        </div>
      </template>
      <template v-if="catalogActions">
        <div class="model-picker-divider" />
        <div class="model-picker-footer-row">
          <button class="model-picker-manage" type="button" @click="handleManageModels">
            {{ t("manageModelsAction") }}
          </button>
          <button
            class="model-picker-refresh"
            type="button"
            :disabled="refreshing"
            v-tooltip.top="refreshing ? t('refreshingModels') : t('refreshModels')"
            :aria-label="refreshing ? t('refreshingModels') : t('refreshModels')"
            @click="handleRefreshModels"
          >
            <svg
              :class="`model-picker-refresh-icon ${refreshing ? 'spinning' : ''}`"
              width="14"
              height="14"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
            >
              <path d="M21 12a9 9 0 1 1-2.64-6.36M21 3v6h-6" />
            </svg>
          </button>
        </div>
      </template>
    </template>
  </Select>
</template>

<script setup lang="ts">
import { computed, inject, onBeforeUnmount, ref } from "vue";
import Select from "primevue/select";
import { statusPickerDt } from "./statusPickerDt";
import { prettyModelLabels } from "../../utils/modelNames";
import {
  defaultThinkingLevelForModel,
  normalizeThinkingLevelForModel,
  supportedThinkingLevels,
  type ThinkingLevel,
} from "./thinkingLevel";
import { useI18n } from "../composables/i18n";
import type { Model } from "../../types";
import { ConversationsListKey } from "../composables/subagentLive";
import { recentModelCombinations, type RecentThinkingLevel } from "./recentModelCombinations";

type PickerModel = Model & { label: string };
type PickerOption = PickerModel & {
  kind: "model" | "recent";
  pickerValue: string;
  recentEffortLabel?: string;
  recentThinkingLevel?: RecentThinkingLevel;
};
type PickerGroup = {
  kind: "models" | "recent";
  label: string;
  items: PickerOption[];
};

const props = withDefaults(
  defineProps<{
    models: Model[];
    selectedModel: string;
    thinkingLevel: ThinkingLevel;
    disabled?: boolean;
    disabledModel?: string;
    showRecent?: boolean;
    showReasoning?: boolean;
    panelClass?: string;
    refreshing?: boolean;
    /** Show the "Manage models…" / refresh footer. Off where the picker
     *  configures a one-shot action rather than the catalog. */
    catalogActions?: boolean;
    /** Portal the overlay to <body> so it isn't clipped by a scrollable
     *  ancestor (e.g. the version modal). */
    appendToBody?: boolean;
    /** Render as bare text (no box, no chevron) for the status-bar readout,
     *  where the picker sits among plain dot-separated segments. The overlay
     *  and behavior are unchanged; only the trigger's chrome differs. */
    inline?: boolean;
    /** Prefix for the combobox's accessible name when the picker configures a
     *  specific action, such as "Rebase model". */
    ariaLabelPrefix?: string;
    /** Fixed text for the trigger instead of the selected model's name, for
     *  callers that name the selection elsewhere and want the trigger to read
     *  as a plain control (e.g. "Model" beside a button naming the model). */
    triggerLabel?: string;
    /** Max height of the option list. The default suits a trigger with the
     *  page below it; a trigger near the bottom of a dialog wants less, so the
     *  panel opens downward instead of flipping over the trigger. */
    scrollHeight?: string;
    /** Why the picker is disabled, put on the combobox itself so a screen
     *  reader that lands on it is told. Pointer users get the caller's tooltip;
     *  ARIA has to come from here because the disabled element is the thing
     *  assistive tech actually focuses. */
    disabledReason?: string;
  }>(),
  {
    disabled: false,
    disabledModel: "",
    showRecent: true,
    showReasoning: true,
    refreshing: false,
    catalogActions: true,
    inline: false,
    appendToBody: false,
    scrollHeight: "22rem",
  },
);
const emit = defineEmits<{
  (e: "selectModel", modelId: string): void;
  (e: "selectCombination", modelId: string, level: RecentThinkingLevel): void;
  (e: "thinkingChange", level: ThinkingLevel): void;
  (e: "manageModels"): void;
  (e: "refreshModels"): void;
}>();

const { t } = useI18n();
const selectRef = ref<InstanceType<typeof Select> | null>(null);
const mobileMq = window.matchMedia("(max-width: 767px)");
const autoFilterFocus = ref(!mobileMq.matches);
const onMobileChange = (event: MediaQueryListEvent) => {
  autoFilterFocus.value = !event.matches;
};
mobileMq.addEventListener("change", onMobileChange);
onBeforeUnmount(() => mobileMq.removeEventListener("change", onMobileChange));
const effortLabelId = `model-picker-effort-label-${Math.random().toString(36).slice(2, 8)}`;
const conversations = inject(ConversationsListKey);
if (!conversations) throw new Error("ModelPicker requires ConversationsListKey");

// ---- model list -------------------------------------------------------

const labels = computed(() => prettyModelLabels(props.models));
const decorated = computed<PickerModel[]>(() =>
  props.models.map((m) => ({ ...m, label: labels.value.get(m.id) || m.id })),
);

// Source sub-labels are pure noise when every model comes from the same
// place — and even in a customized install, the main gateway's name repeated
// under dozens of options drowns out the few models it actually matters for.
// So label only the models whose source differs from the dominant (most
// common) source; in a single-source install that's nothing.
const dominantSource = computed(() => {
  const counts = new Map<string, number>();
  for (const m of props.models) {
    if (!m.source) continue;
    counts.set(m.source, (counts.get(m.source) || 0) + 1);
  }
  let best = "";
  let bestCount = 0;
  for (const [src, n] of counts) {
    if (n > bestCount) {
      best = src;
      bestCount = n;
    }
  }
  return best;
});

// Tier 2 models are overshadowed by a better available sibling; they're kept
// behind an "All models" toggle so the common case stays uncluttered. Absent
// tier is treated as tier 1 (backward compatible with older servers).
const isTier2 = (m: Model) => m.tier === 2;
const tier1Models = computed(() => decorated.value.filter((m) => !isTier2(m)));
const tier2Models = computed(() => decorated.value.filter(isTier2));

const showMore = ref(false);
const filterValue = ref("");

function onFilter(event: { value: string }) {
  filterValue.value = event.value ?? "";
}

// When collapsed, only show tier-1 models — plus the currently selected model
// if it happens to be a tier-2 one, so the selection always renders. While a
// filter is active, search across all models (including tier-2) so hidden
// models remain findable without expanding "All models".
const visibleModels = computed(() => {
  if (showMore.value || filterValue.value) return decorated.value;
  const base = tier1Models.value;
  const selected = decorated.value.find((m) => m.id === props.selectedModel);
  if (selected && isTier2(selected) && !base.includes(selected)) {
    return [...base, selected];
  }
  return base;
});

const recentOptions = computed<PickerOption[]>(() => {
  const byId = new Map(decorated.value.map((model) => [model.id, model]));
  return recentModelCombinations(conversations.value, props.models, Date.now(), 4)
    .filter((combination) => !isActiveRecentCombination(combination))
    .slice(0, 3)
    .flatMap((combination) => {
      const model = byId.get(combination.modelId);
      if (!model) return [];
      return [
        {
          ...model,
          kind: "recent",
          pickerValue: `recent:${model.id}:${combination.thinkingLevel || ""}`,
          recentThinkingLevel: combination.thinkingLevel,
          recentEffortLabel: combination.thinkingLevel || "",
        },
      ];
    });
});

const modelOptions = computed<PickerOption[]>(() =>
  visibleModels.value.map((model) => ({
    ...model,
    kind: "model",
    pickerValue: `model:${model.id}`,
  })),
);

const optionGroups = computed<PickerGroup[]>(() => {
  const groups: PickerGroup[] = [];
  if (props.showRecent && recentOptions.value.length && !filterValue.value) {
    groups.push({ kind: "recent", label: "Recent", items: recentOptions.value });
  }
  groups.push({ kind: "models", label: "", items: modelOptions.value });
  return groups;
});

const pickerOptionsByValue = computed(
  () =>
    new Map(
      [...recentOptions.value, ...modelOptions.value].map((option) => [option.pickerValue, option]),
    ),
);

const selectedPickerValue = computed(() => `model:${props.selectedModel}`);

const selectedModelObj = computed(() => props.models.find((m) => m.id === props.selectedModel));
// selectedModel is "" when the server serves no models (see
// utils/modelSetupHint). Show an explicit placeholder rather than an empty
// control, so the trigger never looks like a model is selected.
const selectedLabel = computed(
  () =>
    labels.value.get(props.selectedModel) || props.selectedModel || t("noModelSelectedPlaceholder"),
);

// ---- reasoning effort --------------------------------------------------

const reasoningSupported = computed(() => selectedModelObj.value?.supports_reasoning !== false);

const modelDefault = computed(() => defaultThinkingLevelForModel(selectedModelObj.value));

// What the pill row highlights. A stored "default" sentinel resolves to the
// model's concrete default level when we know it, so the UI shows the real
// level (e.g. medium) rather than a made-up "default" entry.
const effectiveEffort = computed<ThinkingLevel>(() => {
  const level = normalizeThinkingLevelForModel(props.thinkingLevel, selectedModelObj.value);
  return level === "default" ? modelDefault.value : level;
});

const effortLevels = computed(() => {
  const list = supportedThinkingLevels(selectedModelObj.value).map((value) => ({
    value,
    label: value,
  }));
  // Only keep an "auto" sentinel when the concrete default is unknown, so
  // users can still defer to the model; otherwise the default is just one of
  // the real levels (pre-selected).
  return modelDefault.value === "default"
    ? [{ value: "default" as ThinkingLevel, label: t("effortAuto") }, ...list]
    : list;
});

// Trigger suffix: the concrete effort in play. Blank when the model doesn't
// reason or when the effective level is an unknowable provider default —
// showing nothing beats showing the word "default".
const effortText = computed(() => {
  if (!reasoningSupported.value) return "";
  if (effectiveEffort.value === "default") return "";
  return effectiveEffort.value;
});

// Names the effort even in the inline variant, whose trigger doesn't show it.
const ariaLabel = computed(() => {
  const prefix = props.ariaLabelPrefix || "Model";
  return effortText.value
    ? `${prefix}: ${selectedLabel.value}, reasoning effort: ${effortText.value}`
    : `${prefix}: ${selectedLabel.value}`;
});

// ---- actions -----------------------------------------------------------

function isActiveRecentCombination(combination: {
  modelId: string;
  thinkingLevel: RecentThinkingLevel;
}): boolean {
  if (combination.modelId !== props.selectedModel) return false;
  if (selectedModelObj.value?.supports_reasoning === false) {
    return combination.thinkingLevel === null;
  }
  return (combination.thinkingLevel ?? "default") === effectiveEffort.value;
}

function handleSelect(pickerValue: string) {
  const option = pickerOptionsByValue.value.get(pickerValue);
  if (!option) throw new Error(`unknown model picker option: ${pickerValue}`);
  if (option.kind === "recent") {
    emit("selectCombination", option.id, option.recentThinkingLevel ?? null);
    selectRef.value?.hide();
    return;
  }
  emit("selectModel", option.id);
}

function selectEffort(level: ThinkingLevel) {
  emit("thinkingChange", level);
}

function toggleMore() {
  showMore.value = !showMore.value;
}

function handleManageModels() {
  selectRef.value?.hide();
  emit("manageModels");
}

function handleRefreshModels() {
  emit("refreshModels");
}
</script>
