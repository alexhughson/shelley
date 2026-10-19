<template>
  <svg
    fill="none"
    stroke="currentColor"
    stroke-width="2"
    stroke-linecap="round"
    stroke-linejoin="round"
    viewBox="0 0 24 24"
    aria-hidden="true"
  >
    <template v-if="action === 'diffs'">
      <path d="M4 7h4M6 5v4M14 7h6M4 17h6M14 17h6M17 15v4" />
    </template>
    <template v-else-if="action === 'gitGraph'">
      <path d="M6 16.6V7.4M6 16.6C8 11 12 6 14.6 5" />
      <circle cx="6" cy="5" r="2.4" />
      <circle cx="17" cy="5" r="2.4" />
      <circle cx="6" cy="19" r="2.4" />
    </template>
    <template v-else-if="action === 'commandPalette'">
      <path d="M7 9a2 2 0 1 1 2 -2v10a2 2 0 1 1 -2 -2h10a2 2 0 1 1 -2 2v-10a2 2 0 1 1 2 2h-10" />
    </template>
    <template v-else-if="action === 'terminal'">
      <path d="m4 5 7 7-7 7M14 19h6" />
    </template>
    <template v-else>
      <path d="M13 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h5M13 3v6h6M13 3l6 6" />
      <path d="m14 21 1-4 5-5 3 3-5 5-4 1Z" />
    </template>
  </svg>
</template>

<script setup lang="ts">
defineProps<{
  action: "commandPalette" | "diffs" | "gitGraph" | "terminal" | "editFile";
}>();
</script>
