/**
 * v-reveal — fade a block in when it scrolls into view.
 *
 *   <section v-reveal>…</section>            the block itself
 *   <div v-reveal.stagger>…</div>            its children, one after another
 *
 * The hidden state is added here (before insert), so if IntersectionObserver is
 * missing the content simply stays visible instead of disappearing.
 *
 * threshold is 0, not a fraction: a fraction is a promise about the *element's*
 * size that a long grid cannot keep. A grid of 140 posters is ~6000px tall and
 * sits below a 400px banner, so at most ~4% of it can ever be on screen — under
 * an 0.08 threshold it would never intersect, never get its class, and stay at
 * opacity 0 for good: a full library that renders as an empty page. The bottom
 * rootMargin is what holds the reveal back until the block has really come into
 * view, and it does not depend on how tall the block is.
 */
const OPTIONS = { rootMargin: '0px 0px -12% 0px', threshold: 0 }

export const reveal = {
  beforeMount(el, binding) {
    if (typeof IntersectionObserver === 'undefined') return
    if (window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return
    el.classList.add(binding.modifiers.stagger ? 'reveal-stagger' : 'reveal')
    if (binding.value) el.style.setProperty('--d', `${binding.value}ms`)
  },
  mounted(el) {
    if (!el.classList.contains('reveal') && !el.classList.contains('reveal-stagger')) return
    const io = new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (!e.isIntersecting) continue
        e.target.classList.add('in')
        io.unobserve(e.target)
      }
    }, OPTIONS)
    io.observe(el)
    el._revealObserver = io
  },
  unmounted(el) {
    el._revealObserver?.disconnect()
    delete el._revealObserver
  },
}
