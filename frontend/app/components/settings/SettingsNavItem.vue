<script setup vapor lang="ts">
import { cn } from '../../utils/cn'
import { settingsPagesKey } from '../../composables/settings/useSettingsPages'
/**
 * One section in a SettingsLayout list. The active one is marked as the current page. A `danger`
 * item (Log out) sits apart at the end of the list. On phones, choosing a section opens it.
 */
const props = defineProps<{ icon?: string; active?: boolean; danger?: boolean }>()
const emit = defineEmits<{ click: [event: MouseEvent] }>()
const pages = inject(settingsPagesKey, undefined)
/**
 * One listener: the section changes, then the page, so history records a single step. With two
 * listeners the browser runs pending updates between them and Back lands on a half-way page.
 */
function choose(event: MouseEvent) {
  emit('click', event)
  if (!props.danger) pages?.show()
}
const el = ref<HTMLButtonElement | null>(null)
// Keep the chosen section in view when the list scrolls.
watch(
  () => props.active,
  (active) =>
    active && nextTick(() => el.value?.scrollIntoView({ block: 'nearest', inline: 'nearest' })),
  { immediate: true },
)
</script>
<template>
  <button
    ref="el"
    type="button"
    :class="
      cn(
        'flex min-h-[34px] items-center gap-2.5 rounded-sm px-2.5 py-1 text-left text-[length:var(--ui-text)] text-muted-foreground hover:bg-accent hover:text-foreground max-[700px]:min-h-12 max-[700px]:shrink-0 max-[700px]:gap-3 max-[700px]:rounded-md max-[700px]:px-3 max-[700px]:text-[15px] max-[700px]:text-foreground [@media(pointer:coarse)]:min-h-[44px] max-[700px]:[&>svg:first-child]:text-muted-foreground',
        active &&
          'active bg-selection text-foreground min-[701px]:[&>svg:first-child]:text-primary',
        // On phones the list is a menu of pages; no section is shown as current there.
        active && 'max-[700px]:bg-transparent max-[700px]:hover:bg-accent',
        danger &&
          'settings-logout mt-auto text-destructive hover:bg-destructive hover:text-rail focus-visible:bg-destructive focus-visible:text-rail max-[700px]:mt-3 max-[700px]:text-destructive max-[700px]:[&>svg:first-child]:text-destructive',
      )
    "
    :aria-current="active ? 'page' : undefined"
    @click="choose"
  >
    <AppIcon
      v-if="icon"
      :name="icon"
      :size="17"
    /><slot /><AppIcon
      v-if="!danger && pages"
      name="right"
      :size="16"
      class="ml-auto hidden shrink-0 text-faint max-[700px]:block"
    />
  </button>
</template>
