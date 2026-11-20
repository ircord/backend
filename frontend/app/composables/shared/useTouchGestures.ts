import { isEditable } from '../../../shared/utils/focus'

/**
 * Touch support for the workspace:
 * - Long-press opens the same context menus as right-click (iOS Safari never fires
 *   `contextmenu`; Android does, so the native event is used when it arrives first).
 * - A sideways swipe anywhere in the workspace moves a drawer with the finger: right opens
 *   navigation (or closes the member list), left opens the member list (or closes navigation).
 *   Content that scrolls sideways and text fields keep their own swipes.
 * - On iOS the visual viewport shrinks for the keyboard while 100dvh does not; --app-height
 *   follows the visual viewport so the composer stays above the keyboard.
 */
export function useTouchGestures(options: {
  openNav: () => void
  openMembers: () => void
  closeDrawers: () => void
  /** Whether each drawer can open now (it is not docked, and the view has one). */
  canOpenNav: () => boolean
  canOpenMembers: () => boolean
  navOpen: () => boolean
  membersOpen: () => boolean
}) {
  const LONG_PRESS = 480
  const SLOP = 10
  let timer: ReturnType<typeof setTimeout> | undefined
  let start: { x: number; y: number; target: Element; time: number } | null = null
  let longPressed = false
  let nativeMenu = false
  /** A drawer following the finger: which one, whether it started open, and its width. */
  let drag: {
    el: HTMLElement
    side: 'nav' | 'members'
    wasOpen: boolean
    width: number
    /** Where the drawer edge was when the swipe was recognised, so it doesn't jump. */
    origin: number
    last: { x: number; time: number }
    velocity: number
  } | null = null
  /** Set once a swipe is known to be vertical (scrolling) or claimed by other content. */
  let ignored = false

  function cancel() {
    clearTimeout(timer)
    timer = undefined
  }
  function pointerCancel() {
    cancel()
    if (drag) settle(drag.wasOpen)
    start = null
    longPressed = false
  }
  function pointerDown(event: PointerEvent) {
    if (event.pointerType !== 'touch') return
    start = {
      x: event.clientX,
      y: event.clientY,
      target: event.target as Element,
      time: Date.now(),
    }
    longPressed = false
    nativeMenu = false
    ignored = false
    cancel()
    // Text inputs keep their own long-press (selection, paste).
    if (isEditable(event.target)) return
    timer = setTimeout(() => {
      if (!start || nativeMenu) return
      longPressed = true
      navigator.vibrate?.(10)
      start.target.dispatchEvent(
        new MouseEvent('contextmenu', {
          bubbles: true,
          cancelable: true,
          clientX: start.x,
          clientY: start.y,
        }),
      )
    }, LONG_PRESS)
  }
  /** True when the swipe belongs to what it started on: a field, a slider or sideways scroll. */
  function claimed(target: Element) {
    // An open drawer is itself a dialog on phones; its swipe closes it.
    const dialog = '[role="dialog"]:not(.conversation-sidebar, .member-sidebar)'
    if (isEditable(target) || target.closest(`${dialog}, [role="slider"], .network-rail`))
      return true
    for (let el: Element | null = target; el && el !== document.body; el = el.parentElement) {
      const overflow = getComputedStyle(el).overflowX
      if ((overflow === 'auto' || overflow === 'scroll') && el.scrollWidth > el.clientWidth + 1)
        return true
    }
    return false
  }
  /** The drawer this sideways swipe moves, if any. */
  function begin(dx: number) {
    const navOpen = options.navOpen()
    const membersOpen = options.membersOpen()
    const side: 'nav' | 'members' | false = navOpen
      ? dx < 0 && 'nav'
      : membersOpen
        ? dx > 0 && 'members'
        : dx > 0
          ? options.canOpenNav() && 'nav'
          : options.canOpenMembers() && 'members'
    if (!side) return null
    const el = document.querySelector<HTMLElement>(
      side === 'nav' ? '.conversation-sidebar' : '.member-sidebar',
    )
    if (!el || getComputedStyle(el).position !== 'fixed') return null
    const wasOpen = side === 'nav' ? navOpen : membersOpen
    const width = el.offsetWidth
    el.style.transition = 'none'
    el.style.visibility = 'visible'
    return { el, side, wasOpen, width, origin: dx, last: { x: dx, time: Date.now() }, velocity: 0 }
  }
  /** How far open the drawer is, 0 to 1, for a finger `dx` from where the swipe started. */
  function progress(d: NonNullable<typeof drag>, dx: number) {
    const moved = (dx - d.origin) / d.width
    const towardOpen = d.side === 'nav' ? moved : -moved
    return Math.min(1, Math.max(0, (d.wasOpen ? 1 : 0) + towardOpen))
  }
  function place(d: NonNullable<typeof drag>, amount: number) {
    const hidden = (1 - amount) * 100
    d.el.style.transform = `translateX(${d.side === 'nav' ? -hidden : hidden}%)`
  }
  /** Finishes a drag: animates to open or closed, then hands back to the workspace state. */
  function settle(open: boolean) {
    const d = drag
    drag = null
    if (!d) return
    const reduced = document.documentElement.dataset.motion === 'reduced'
    d.el.style.transition = reduced ? 'none' : 'transform 0.18s ease-out'
    place(d, open ? 1 : 0)
    if (open && d.side === 'nav') options.openNav()
    else if (open) options.openMembers()
    else if (d.wasOpen) options.closeDrawers()
    setTimeout(
      () => {
        d.el.style.removeProperty('transition')
        d.el.style.removeProperty('transform')
        d.el.style.removeProperty('visibility')
      },
      reduced ? 0 : 200,
    )
  }
  function pointerMove(event: PointerEvent) {
    if (!start || event.pointerType !== 'touch') return
    const dx = event.clientX - start.x
    const dy = event.clientY - start.y
    if (Math.hypot(dx, dy) > SLOP) cancel()
    if (drag) {
      const now = Date.now()
      if (now > drag.last.time) drag.velocity = (dx - drag.last.x) / (now - drag.last.time)
      drag.last = { x: dx, time: now }
      place(drag, progress(drag, dx))
      return
    }
    if (ignored || longPressed || Math.hypot(dx, dy) <= SLOP) return
    // Mostly vertical is scrolling; it stays that way for the rest of the touch.
    if (Math.abs(dx) < Math.abs(dy) * 1.2 || claimed(start.target)) {
      ignored = true
      return
    }
    drag = begin(dx)
    if (!drag) ignored = true
  }
  function pointerUp(event: PointerEvent) {
    if (event.pointerType !== 'touch' || !start) return
    cancel()
    if (drag) {
      const amount = progress(drag, event.clientX - start.x)
      // A flick decides by direction; otherwise past a third open stays open.
      const toward = drag.side === 'nav' ? drag.velocity : -drag.velocity
      settle(Math.abs(drag.velocity) > 0.4 ? toward > 0 : amount > (drag.wasOpen ? 2 / 3 : 1 / 3))
    }
    start = null
  }
  // A native long-press menu (Android) wins; ours is then skipped, and vice versa.
  function contextMenu(event: MouseEvent) {
    if (!event.isTrusted) return
    if (longPressed) {
      event.preventDefault()
      event.stopImmediatePropagation()
    } else nativeMenu = true
  }
  // The finger lifting after a long-press must not also click whatever is underneath.
  function click(event: MouseEvent) {
    if (!longPressed) return
    longPressed = false
    event.preventDefault()
    event.stopImmediatePropagation()
  }

  function viewport() {
    const vv = window.visualViewport
    // Pinch zoom must magnify the workspace, rather than relayout it to the zoomed viewport.
    if (!vv || vv.scale !== 1) return
    document.documentElement.style.setProperty('--app-height', `${Math.round(vv.height)}px`)
    // iOS scrolls the page to reveal the focused input; the layout already did that.
    if (window.scrollY) window.scrollTo(0, 0)
  }

  useEventListener(document, 'pointerdown', pointerDown, { passive: true })
  useEventListener(document, 'pointermove', pointerMove, { passive: true })
  useEventListener(document, 'pointerup', pointerUp, { passive: true })
  useEventListener(document, 'pointercancel', pointerCancel, { passive: true })
  useEventListener(document, 'contextmenu', contextMenu, { capture: true })
  useEventListener(document, 'click', click, { capture: true })
  useEventListener(window.visualViewport, 'resize', viewport)
  onMounted(viewport)
  onBeforeUnmount(() => {
    cancel()
    document.documentElement.style.removeProperty('--app-height')
  })
}
