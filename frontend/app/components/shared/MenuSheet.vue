<!-- Not Vapor: Reka's dialog parts merge props into VNode slots. -->
<script setup lang="ts">
import {
  DialogContent,
  DialogDescription,
  DialogOverlay,
  DialogPortal,
  DialogRoot,
  DialogTitle,
} from 'reka-ui'
import type { MenuItem, QuickAction } from '../../composables/shared/useContextMenu'

/**
 * The shared menu on phones: a sheet from the bottom with the same items, grouped into cards.
 * Drag it down, tap outside, press Escape or Back to close it. A submenu opens as a second page.
 */
const { t } = useI18n()
const { state, close } = useContextMenu()
const sub = ref<MenuItem | null>(null)
watch(
  () => state.items,
  () => (sub.value = null),
)
/** Separators split the items into cards. */
const groups = computed(() => {
  const out: MenuItem[][] = [[]]
  for (const entry of sub.value?.children ?? state.items) {
    if (entry !== '-') out.at(-1)!.push(entry)
    else if (out.at(-1)!.length) out.push([])
  }
  return out.filter((group) => group.length)
})
const title = computed(() => sub.value?.label ?? state.title)
/** An unavailable item with a reason shows the reason; choosing it does nothing. */
const explained = (item: MenuItem) => !!item.disabled && !!item.title

// Back closes the sheet, like a dialog.
const unregister = useDialogStack().register(() => close(false))
onBeforeUnmount(unregister)

const content = ref<{ $el: HTMLElement } | null>(null)
const sheet = computed(() => content.value?.$el ?? null)
const scroller = ref<HTMLElement | null>(null)
const leaving = ref(false)
/** Slides the sheet away, then closes the menu. */
function dismiss() {
  if (leaving.value) return
  leaving.value = true
  if (sheet.value) sheet.value.style.transform = ''
  const reduced = document.documentElement.dataset.motion === 'reduced'
  setTimeout(() => close(false), reduced ? 0 : 180)
}
function choose(item: MenuItem | QuickAction) {
  if ('children' in item && item.children) {
    sub.value = item
    scroller.value?.scrollTo({ top: 0 })
    return
  }
  if ('disabled' in item && item.disabled) return
  close()
  item.action?.()
}
function openChange(open: boolean) {
  if (!open) dismiss()
}
function focusSheet(event: Event) {
  event.preventDefault()
  sheet.value?.focus({ preventScroll: true })
}

// Dragging down from the top of the list moves the sheet; far enough or fast enough closes it.
let drag: { y: number; x: number; time: number; moved: number; active: boolean } | null = null
function touchStart(event: TouchEvent) {
  const touch = event.touches[0]
  if (!touch || event.touches.length > 1 || leaving.value) return
  drag = { y: touch.clientY, x: touch.clientX, time: Date.now(), moved: 0, active: false }
}
function touchMove(event: TouchEvent) {
  const touch = event.touches[0]
  if (!drag || !touch || !sheet.value) return
  const dy = touch.clientY - drag.y
  if (!drag.active) {
    const atTop = (scroller.value?.scrollTop ?? 0) <= 0
    if (dy < 8 || !atTop || Math.abs(touch.clientX - drag.x) > dy) {
      if (Math.abs(dy) > 8 || Math.abs(touch.clientX - drag.x) > 8) drag = null
      return
    }
    drag.active = true
    drag.y = touch.clientY
    drag.time = Date.now()
    sheet.value.style.transition = 'none'
  }
  event.preventDefault()
  drag.moved = Math.max(0, touch.clientY - drag.y)
  sheet.value.style.transform = `translateY(${drag.moved}px)`
}
function touchEnd() {
  const d = drag
  drag = null
  if (!d?.active || !sheet.value) return
  sheet.value.style.transition = ''
  const fast = d.moved / Math.max(1, Date.now() - d.time) > 0.5
  if (fast || d.moved > sheet.value.offsetHeight / 4) dismiss()
  else sheet.value.style.transform = ''
}
useEventListener(sheet, 'touchstart', touchStart, { passive: true })
useEventListener(sheet, 'touchmove', touchMove, { passive: false })
useEventListener(sheet, 'touchend', touchEnd, { passive: true })
useEventListener(sheet, 'touchcancel', touchEnd, { passive: true })
</script>
<template>
  <DialogRoot
    :open="true"
    @update:open="openChange"
  >
    <DialogPortal>
      <DialogOverlay
        class="menu-sheet-overlay fixed inset-0 z-[70] bg-[rgb(5_5_10_/_55%)]"
        :data-leaving="leaving || undefined"
      />
      <DialogContent
        ref="content"
        class="menu-sheet fixed inset-x-0 bottom-0 z-[70] flex max-h-[85dvh] flex-col rounded-t-2xl bg-popover text-popover-foreground shadow-[0_-8px_32px_rgb(0_0_0_/_35%),inset_0_1px_0_color-mix(in_srgb,var(--text)_6%,transparent)] outline-none"
        :data-leaving="leaving || undefined"
        @open-auto-focus="focusSheet"
        @close-auto-focus.prevent
        @contextmenu.prevent.stop
      >
        <span
          class="mx-auto mt-2 mb-1 h-1 w-9 shrink-0 rounded-full bg-foreground/25"
          aria-hidden="true"
        />
        <header class="flex min-h-11 shrink-0 items-center gap-1 px-4 pb-2">
          <button
            v-if="sub"
            type="button"
            class="-ml-2 grid size-9 shrink-0 place-items-center rounded-full text-muted-foreground active:bg-foreground/8"
            :aria-label="
              t('settings.backTo', { name: state.title || t('workspaceUi.shell.actions') })
            "
            @click="sub = null"
          >
            <AppIcon
              name="back"
              :size="20"
            />
          </button>
          <div class="min-w-0 flex-1">
            <DialogTitle
              class="truncate text-[length:calc(16px_*_var(--interface-scale,1))] font-[650]"
              :class="{ 'sr-only': !title }"
              >{{ title || t('workspaceUi.shell.actions') }}</DialogTitle
            >
            <DialogDescription
              v-if="state.detail && !sub"
              class="truncate text-[length:var(--ui-small)] text-muted-foreground"
              >{{ state.detail }}</DialogDescription
            >
          </div>
        </header>
        <div
          ref="scroller"
          class="flex min-h-0 flex-col gap-3 overflow-y-auto overscroll-contain px-3 pb-[calc(12px_+_env(safe-area-inset-bottom))]"
        >
          <div
            v-if="state.quick.length && !sub"
            class="flex gap-2"
          >
            <button
              v-for="quick in state.quick"
              :key="quick.emoji"
              type="button"
              class="grid h-12 min-w-0 flex-1 place-items-center rounded-full bg-foreground/6 text-[24px] transition-transform active:scale-110 active:bg-primary/20 [&_img]:size-7"
              :aria-label="quick.label"
              @click="choose(quick)"
            >
              <CustomEmoji
                v-if="quick.item?.url"
                :item="quick.item"
                icon
              /><template v-else>{{ quick.emoji }}</template>
            </button>
          </div>
          <div
            v-for="(group, groupIndex) in groups"
            :key="`${sub?.label ?? ''}-${groupIndex}`"
            class="overflow-hidden rounded-xl bg-foreground/5"
          >
            <button
              v-for="(item, index) in group"
              :key="index"
              type="button"
              class="menu-sheet-item flex min-h-12 w-full items-center gap-3 px-3 py-2 text-left text-[length:calc(15px_*_var(--interface-scale,1))] font-[500] active:bg-primary/14 disabled:opacity-45 [&+&]:border-t [&+&]:border-solid [&+&]:border-[color-mix(in_srgb,var(--border)_50%,transparent)]"
              :class="item.danger ? 'text-destructive active:bg-destructive/14' : 'text-body'"
              :disabled="item.disabled && !explained(item)"
              :aria-disabled="explained(item) || undefined"
              :aria-pressed="item.checked"
              @click="choose(item)"
            >
              <span
                v-if="item.icon"
                class="grid size-8 shrink-0 place-items-center rounded-lg"
                :class="item.danger ? 'bg-destructive/14' : 'bg-foreground/7 text-muted-foreground'"
                aria-hidden="true"
              >
                <AppIcon
                  :name="item.icon"
                  :size="17"
                />
              </span>
              <span
                class="flex min-w-0 flex-1 flex-col"
                :class="{ 'opacity-45': explained(item) }"
              >
                <span class="truncate">{{ item.label }}</span>
                <span
                  v-if="explained(item)"
                  class="text-[length:var(--ui-small)] font-[400] text-muted-foreground"
                  >{{ item.title }}</span
                >
              </span>
              <span
                v-if="item.hint && !explained(item)"
                class="shrink-0 text-[length:var(--ui-small)] text-muted-foreground"
                >{{ item.hint }}</span
              >
              <AppIcon
                v-if="item.children"
                name="right"
                :size="16"
                class="shrink-0 text-muted-foreground"
              />
              <AppIcon
                v-else-if="item.checked"
                name="check"
                :size="18"
                class="shrink-0 text-primary"
              />
            </button>
          </div>
        </div>
      </DialogContent>
    </DialogPortal>
  </DialogRoot>
</template>
<style>
.menu-sheet {
  animation: menu-sheet-up 0.22s cubic-bezier(0.2, 0.8, 0.3, 1);
  transition: transform 0.18s ease-out;
}
.menu-sheet[data-leaving] {
  transform: translateY(100%) !important;
}
.menu-sheet-overlay {
  animation: menu-sheet-fade 0.2s ease-out;
  transition: opacity 0.18s ease-out;
}
.menu-sheet-overlay[data-leaving] {
  opacity: 0;
}
@keyframes menu-sheet-up {
  from {
    transform: translateY(100%);
  }
}
@keyframes menu-sheet-fade {
  from {
    opacity: 0;
  }
}
:root[data-motion='reduced'] :is(.menu-sheet, .menu-sheet-overlay) {
  animation: none;
  transition: none;
}
</style>
