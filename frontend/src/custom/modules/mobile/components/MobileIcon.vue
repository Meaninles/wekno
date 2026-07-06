<script setup lang="ts">
defineOptions({ name: "MobileIcon" });

defineProps<{
  name: string;
}>();

const is = (current: string, names: string[]) => names.includes(current);
</script>

<template>
  <svg
    class="mobile-icon"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="2"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <template v-if="is(name, ['menu'])">
      <path d="M4 7h16" />
      <path d="M4 12h16" />
      <path d="M4 17h16" />
    </template>

    <template v-else-if="is(name, ['setting'])">
      <path d="M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Z" />
      <path d="M19.4 15a1.7 1.7 0 0 0 .34 1.88l.04.04a2 2 0 1 1-2.83 2.83l-.04-.04a1.7 1.7 0 0 0-1.88-.34 1.7 1.7 0 0 0-1.03 1.56V21a2 2 0 1 1-4 0v-.06A1.7 1.7 0 0 0 8.97 19.4a1.7 1.7 0 0 0-1.88.34l-.04.04a2 2 0 1 1-2.83-2.83l.04-.04A1.7 1.7 0 0 0 4.6 15 1.7 1.7 0 0 0 3.06 14H3a2 2 0 1 1 0-4h.06A1.7 1.7 0 0 0 4.6 9a1.7 1.7 0 0 0-.34-1.88l-.04-.04a2 2 0 1 1 2.83-2.83l.04.04A1.7 1.7 0 0 0 8.97 4.6 1.7 1.7 0 0 0 10 3.06V3a2 2 0 1 1 4 0v.06A1.7 1.7 0 0 0 15.03 4.6a1.7 1.7 0 0 0 1.88-.34l.04-.04a2 2 0 1 1 2.83 2.83l-.04.04A1.7 1.7 0 0 0 19.4 9c.2.6.78 1 1.54 1H21a2 2 0 1 1 0 4h-.06A1.7 1.7 0 0 0 19.4 15Z" />
    </template>

    <template v-else-if="is(name, ['chevron-left'])">
      <path d="m15 18-6-6 6-6" />
    </template>
    <template v-else-if="is(name, ['chevron-right'])">
      <path d="m9 18 6-6-6-6" />
    </template>
    <template v-else-if="is(name, ['chevron-up'])">
      <path d="m18 15-6-6-6 6" />
    </template>
    <template v-else-if="is(name, ['chevron-down'])">
      <path d="m6 9 6 6 6-6" />
    </template>

    <template v-else-if="is(name, ['send'])">
      <path d="m22 2-7 20-4-9-9-4Z" />
      <path d="M22 2 11 13" />
    </template>
    <template v-else-if="is(name, ['stop-circle'])">
      <circle cx="12" cy="12" r="9" />
      <path d="M9 9h6v6H9z" />
    </template>
    <template v-else-if="is(name, ['plus-circle'])">
      <circle cx="12" cy="12" r="9" />
      <path d="M12 8v8" />
      <path d="M8 12h8" />
    </template>

    <template v-else-if="is(name, ['folder'])">
      <path d="M3 6.5A2.5 2.5 0 0 1 5.5 4H10l2 2h6.5A2.5 2.5 0 0 1 21 8.5v8A2.5 2.5 0 0 1 18.5 19h-13A2.5 2.5 0 0 1 3 16.5Z" />
    </template>
    <template v-else-if="is(name, ['file'])">
      <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8Z" />
      <path d="M14 3v5h5" />
    </template>
    <template v-else-if="is(name, ['file-search'])">
      <path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h5" />
      <path d="M14 3v5h5" />
      <circle cx="16.5" cy="16.5" r="2.5" />
      <path d="m19 19 2 2" />
    </template>

    <template v-else-if="is(name, ['upload'])">
      <path d="M12 16V4" />
      <path d="m7 9 5-5 5 5" />
      <path d="M4 20h16" />
    </template>
    <template v-else-if="is(name, ['download'])">
      <path d="M12 4v12" />
      <path d="m7 11 5 5 5-5" />
      <path d="M4 20h16" />
    </template>
    <template v-else-if="is(name, ['delete'])">
      <path d="M4 7h16" />
      <path d="M10 11v6" />
      <path d="M14 11v6" />
      <path d="M6 7l1 14h10l1-14" />
      <path d="M9 7V4h6v3" />
    </template>
    <template v-else-if="is(name, ['refresh'])">
      <path d="M20 11a8 8 0 0 0-14.9-4" />
      <path d="M4 7h5V2" />
      <path d="M4 13a8 8 0 0 0 14.9 4" />
      <path d="M20 17h-5v5" />
    </template>

    <template v-else-if="is(name, ['image'])">
      <rect x="3" y="5" width="18" height="14" rx="2" />
      <circle cx="8.5" cy="10" r="1.5" />
      <path d="m21 16-5-5L5 19" />
    </template>
    <template v-else-if="is(name, ['attach'])">
      <path d="m21.4 11.6-8.5 8.5a5 5 0 0 1-7.1-7.1l9.2-9.2a3.5 3.5 0 0 1 5 5l-9.2 9.2a2 2 0 1 1-2.8-2.8l8.5-8.5" />
    </template>
    <template v-else-if="is(name, ['internet'])">
      <circle cx="12" cy="12" r="9" />
      <path d="M3 12h18" />
      <path d="M12 3a14 14 0 0 1 0 18" />
      <path d="M12 3a14 14 0 0 0 0 18" />
    </template>

    <template v-else-if="is(name, ['tag'])">
      <path d="M20.6 13.4 13.4 20.6a2 2 0 0 1-2.8 0L3 13V3h10l7.6 7.6a2 2 0 0 1 0 2.8Z" />
      <circle cx="8" cy="8" r="1" />
    </template>
    <template v-else-if="is(name, ['bookmark'])">
      <path d="M6 4a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v18l-6-4-6 4Z" />
    </template>
    <template v-else-if="is(name, ['lightbulb'])">
      <path d="M9 18h6" />
      <path d="M10 22h4" />
      <path d="M8.5 14.5A6 6 0 1 1 15.5 14.5c-.9.7-1.5 1.6-1.5 2.5h-4c0-.9-.6-1.8-1.5-2.5Z" />
    </template>
    <template v-else-if="is(name, ['education'])">
      <path d="m22 10-10-5-10 5 10 5 10-5Z" />
      <path d="M6 12v5c3 2 9 2 12 0v-5" />
    </template>
    <template v-else-if="is(name, ['pin'])">
      <path d="M12 17v5" />
      <path d="m5 12 7-7 7 7" />
      <path d="M7 12h10" />
      <path d="M9 12v5h6v-5" />
    </template>
    <template v-else-if="is(name, ['pin-filled'])">
      <path d="M12 17v5" />
      <path d="M5 12h14L12 5Z" fill="currentColor" stroke="none" />
      <path d="M9 12v5h6v-5" fill="currentColor" stroke="none" />
    </template>
    <template v-else-if="is(name, ['tools'])">
      <path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.5 2.5-2.8-2.8Z" />
    </template>
    <template v-else-if="is(name, ['cpu'])">
      <rect x="7" y="7" width="10" height="10" rx="2" />
      <path d="M9 1v3M15 1v3M9 20v3M15 20v3M1 9h3M1 15h3M20 9h3M20 15h3" />
    </template>
    <template v-else-if="is(name, ['user-talk'])">
      <path d="M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z" />
      <path d="M2 21a7 7 0 0 1 10.5-6.1" />
      <path d="M15 14h6v5h-3l-3 3Z" />
    </template>
    <template v-else-if="is(name, ['close'])">
      <path d="M18 6 6 18" />
      <path d="m6 6 12 12" />
    </template>

    <template v-else>
      <rect x="4" y="4" width="16" height="16" rx="4" />
      <path d="M8 12h8" />
    </template>
  </svg>
</template>

<style scoped>
.mobile-icon {
  display: inline-block;
  width: 1em;
  height: 1em;
  flex: 0 0 auto;
}
</style>
