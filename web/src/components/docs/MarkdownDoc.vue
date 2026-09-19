<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { copyToClipboard } from 'quasar'
import { notify } from '@/api/client'
import { annotateHeadings, guideHref, renderMarkdown, type DocHeading } from '@/docs/markdown'

const props = defineProps<{
  /** Текст руководства. */
  markdown: string
  /** Руководство, к которому относятся якоря на странице. */
  slug: string
}>()

const emit = defineEmits<{
  /** Заголовки второго и третьего уровня — для оглавления справа. */
  (e: 'headings', headings: DocHeading[]): void
  /** Раздел, который сейчас читают. */
  (e: 'active', id: string): void
  /** Текст отрисован, id заголовков проставлены — можно прокручивать к якорю. */
  (e: 'rendered'): void
  /** Щелчок по якорю внутри этого же руководства. Адрес меняет страница. */
  (e: 'anchor', id: string): void
}>()

const router = useRouter()
const root = ref<HTMLElement | null>(null)
const html = computed(() => renderMarkdown(props.markdown))
let headingEls: HTMLElement[] = []

function afterRender() {
  const el = root.value
  if (!el) return
  const headings = annotateHeadings(el, true)
  headingEls = Array.from(el.querySelectorAll<HTMLElement>('h2, h3'))
  emit('headings', headings.filter((h) => h.level === 2 || h.level === 3))
  emit('rendered')
  lastActive = ''
  onScroll()
}

watch(html, () => nextTick(afterRender))
onMounted(afterRender)

/** Прокрутка к разделу. Отступ под шапку задаёт scroll-margin-top в стилях. */
function scrollToHeading(id: string, smooth = true) {
  const target = root.value?.querySelector<HTMLElement>(`[id="${CSS.escape(id)}"]`)
  target?.scrollIntoView({ behavior: smooth ? 'smooth' : 'auto', block: 'start' })
  return Boolean(target)
}

defineExpose({ scrollToHeading })

function withModifier(event: MouseEvent): boolean {
  return event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey
}

async function copyCode(button: HTMLElement) {
  const code = button.closest('.md-code')?.querySelector('pre code')?.textContent ?? ''
  try {
    await copyToClipboard(code.replace(/\n$/, ''))
    const label = button.querySelector('.md-code__copy-label')
    const icon = button.querySelector('.material-icons')
    button.classList.add('is-done')
    if (label) label.textContent = 'Скопировано'
    if (icon) icon.textContent = 'check'
    window.setTimeout(() => {
      button.classList.remove('is-done')
      if (label) label.textContent = 'Копировать'
      if (icon) icon.textContent = 'content_copy'
    }, 1600)
  } catch {
    notify({ type: 'warning', message: 'Браузер не дал доступ к буферу обмена — выделите код вручную' })
  }
}

function onClick(event: MouseEvent) {
  const target = event.target as HTMLElement | null
  if (!target) return

  const copy = target.closest<HTMLElement>('[data-md-copy]')
  if (copy) {
    event.preventDefault()
    void copyCode(copy)
    return
  }

  const heading = target.closest<HTMLElement>('[data-md-heading]')
  if (heading) {
    event.preventDefault()
    const id = heading.dataset.mdHeading ?? ''
    emit('anchor', id)
    copyToClipboard(window.location.origin + guideHref(props.slug, id))
      .then(() => notify({ type: 'positive', message: 'Ссылка на раздел скопирована', timeout: 1800 }))
      .catch(() => undefined)
    return
  }

  const link = target.closest<HTMLAnchorElement>('a[data-md-guide], a[data-md-target]')
  if (!link || withModifier(event)) return
  event.preventDefault()
  const slug = link.dataset.mdGuide ?? props.slug
  const anchor = link.dataset.mdTarget
  if (slug === props.slug) {
    if (anchor) emit('anchor', anchor)
    return
  }
  void router.push({ name: 'documentation', params: { doc: slug }, hash: anchor ? `#${anchor}` : '' })
}

// Какой раздел сейчас на экране: последний заголовок, ушедший под шапку.
//
// Считается прямо в обработчике: браузер и так присылает scroll не чаще раза в
// кадр, а откладывать расчёт на requestAnimationFrame опасно — в свёрнутой или
// перекрытой вкладке кадры не рисуются, и подсветка застревала бы.
let lastActive = ''
function onScroll() {
  let active = ''
  for (const el of headingEls) {
    if (el.getBoundingClientRect().top <= 120) active = el.id
    else break
  }
  active ||= headingEls[0]?.id ?? ''
  if (active !== lastActive) {
    lastActive = active
    emit('active', active)
  }
}

onMounted(() => window.addEventListener('scroll', onScroll, { passive: true }))
onBeforeUnmount(() => window.removeEventListener('scroll', onScroll))
</script>

<template>
  <!-- HTML собран из встроенного Markdown; сырой HTML в нём экранирован. -->
  <article ref="root" class="md-body" @click="onClick" v-html="html" />
</template>

<style lang="scss">
// Стили не scoped: содержимое приходит через v-html, и scoped-атрибуты на него
// не попадают. Всё ограничено классом .md-body, наружу ничего не течёт.
.md-body {
  --md-code-bg: #f6f8fa;
  --md-code-border: #d8dee4;
  --md-inline-code-bg: rgba(27, 31, 35, 0.07);
  --md-mono: ui-monospace, SFMono-Regular, 'Cascadia Mono', Menlo, Consolas, 'Liberation Mono', monospace;

  font-size: 15.5px;
  line-height: 1.72;
  overflow-wrap: break-word;
  min-width: 0;

  > :first-child { margin-top: 0; }

  h1, h2, h3, h4, h5, h6 {
    position: relative;
    scroll-margin-top: 80px;
    line-height: 1.3;
    font-weight: 650;
    letter-spacing: -0.005em;
  }
  h1 { font-size: 30px; margin: 0 0 18px; letter-spacing: -0.015em; }
  h2 {
    font-size: 22px;
    margin: 44px 0 14px;
    padding-bottom: 8px;
    border-bottom: 1px solid var(--jhv-border);
  }
  h3 { font-size: 18px; margin: 30px 0 10px; }
  h4 { font-size: 16px; margin: 24px 0 8px; }
  h5, h6 { font-size: 15px; margin: 20px 0 6px; color: var(--jhv-text-muted); }

  .md-heading-anchor {
    margin-left: 10px;
    color: var(--jhv-text-subtle);
    text-decoration: none;
    font-weight: 400;
    opacity: 0;
    transition: opacity 0.15s;
  }
  h2:hover .md-heading-anchor,
  h3:hover .md-heading-anchor,
  h4:hover .md-heading-anchor,
  .md-heading-anchor:focus-visible { opacity: 1; }

  p { margin: 0 0 14px; }
  p:empty { display: none; }

  ul, ol { margin: 0 0 16px; padding-left: 26px; }
  li { margin: 4px 0; }
  li > p { margin: 0 0 6px; }
  li > ul, li > ol { margin: 4px 0 0; }
  li:has(> input[type='checkbox']) { list-style: none; margin-left: -22px; }
  input[type='checkbox'] { margin-right: 8px; vertical-align: -1px; }

  a {
    color: var(--q-primary);
    text-decoration: none;
    border-bottom: 1px solid transparent;
    &:hover { border-bottom-color: currentColor; }
  }
  .md-external { font-size: 14px; margin-left: 2px; vertical-align: -2px; opacity: 0.75; }

  .md-srcref {
    font-family: var(--md-mono);
    font-size: 0.86em;
    border-bottom: 1px dashed var(--jhv-text-subtle);
    cursor: help;
  }

  strong { font-weight: 650; }

  code {
    font-family: var(--md-mono);
    font-size: 0.86em;
    background: var(--md-inline-code-bg);
    padding: 0.15em 0.4em;
    border-radius: 5px;
  }

  hr { border: 0; border-top: 1px solid var(--jhv-border); margin: 36px 0; }

  // Блоки кода: подпись языка и кнопка «Копировать».
  .md-code, .md-diagram {
    margin: 16px 0 20px;
    border: 1px solid var(--md-code-border);
    border-radius: 10px;
    overflow: hidden;
    background: var(--md-code-bg);
  }
  .md-code__bar, .md-diagram__caption {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 6px 4px 14px;
    min-height: 34px;
    font-size: 12px;
    color: var(--jhv-text-subtle);
    border-bottom: 1px solid var(--md-code-border);
  }
  .md-code__lang { font-weight: 600; letter-spacing: 0.04em; text-transform: uppercase; }
  .md-code__copy {
    margin-left: auto;
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 3px 10px;
    font: inherit;
    color: inherit;
    background: transparent;
    border: 1px solid transparent;
    border-radius: 6px;
    cursor: pointer;
    .material-icons { font-size: 15px; }
    &:hover { border-color: var(--md-code-border); color: var(--q-primary); }
    &:focus-visible { outline: 2px solid var(--q-primary); outline-offset: 1px; }
    &.is-done { color: var(--q-positive); }
  }
  .md-diagram__caption .material-icons { font-size: 16px; color: var(--q-primary); }
  pre {
    margin: 0;
    padding: 14px 16px;
    overflow-x: auto;
    font-size: 13.2px;
    line-height: 1.6;
    tab-size: 4;
  }
  pre code { background: none; padding: 0; font-size: inherit; border-radius: 0; white-space: pre; }

  // Таблицы: прокрутка вбок вместо разъезжающейся страницы.
  .md-table {
    margin: 16px 0 20px;
    overflow-x: auto;
    border: 1px solid var(--jhv-border);
    border-radius: 10px;
  }
  table { width: 100%; border-collapse: collapse; font-size: 14px; line-height: 1.55; }
  th, td {
    padding: 9px 14px;
    text-align: left;
    vertical-align: top;
    border-bottom: 1px solid var(--jhv-border);
  }
  th { background: var(--jhv-surface-muted); font-weight: 600; white-space: nowrap; }
  tr:last-child td { border-bottom: 0; }
  tbody tr:hover td { background: var(--jhv-surface-muted); }

  // Цитаты и предупреждения.
  .md-quote {
    margin: 16px 0 20px;
    padding: 10px 18px;
    border-left: 4px solid var(--q-primary);
    background: var(--jhv-surface-info);
    border-radius: 0 10px 10px 0;
    > :last-child { margin-bottom: 0; }
  }
  .md-callout {
    margin: 16px 0 20px;
    padding: 12px 18px 12px 16px;
    border-left: 4px solid var(--md-callout-accent);
    background: var(--md-callout-bg);
    border-radius: 0 10px 10px 0;
    > :last-child { margin-bottom: 0; }
  }
  .md-callout__title {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 4px;
    font-weight: 650;
    color: var(--md-callout-accent);
    .material-icons { font-size: 19px; }
  }
  .md-callout--note { --md-callout-accent: #1f6feb; --md-callout-bg: var(--jhv-surface-info); }
  .md-callout--tip { --md-callout-accent: #1a7f37; --md-callout-bg: var(--jhv-surface-success); }
  .md-callout--important { --md-callout-accent: #8250df; --md-callout-bg: rgba(130, 80, 223, 0.08); }
  .md-callout--warning { --md-callout-accent: #b35900; --md-callout-bg: var(--jhv-surface-warning); }
  .md-callout--caution { --md-callout-accent: #cf222e; --md-callout-bg: var(--jhv-surface-danger); }

  // Подсветка синтаксиса — палитра в духе GitHub.
  .hljs-comment, .hljs-quote { color: #6e7781; font-style: italic; }
  .hljs-keyword, .hljs-selector-tag, .hljs-built_in, .hljs-meta .hljs-keyword { color: #cf222e; }
  .hljs-string, .hljs-regexp, .hljs-addition, .hljs-meta .hljs-string { color: #0a3069; }
  .hljs-number, .hljs-literal, .hljs-symbol, .hljs-bullet { color: #0550ae; }
  .hljs-title, .hljs-title.function_, .hljs-section { color: #8250df; }
  .hljs-attr, .hljs-attribute, .hljs-variable, .hljs-template-variable, .hljs-property { color: #0550ae; }
  .hljs-name, .hljs-tag, .hljs-selector-class, .hljs-selector-id { color: #116329; }
  .hljs-meta, .hljs-params { color: #953800; }
  .hljs-deletion { color: #82071e; background: #ffebe9; }
  .hljs-emphasis { font-style: italic; }
  .hljs-strong { font-weight: 700; }
}

body.body--dark .md-body {
  --md-code-bg: #161b22;
  --md-code-border: #30363d;
  --md-inline-code-bg: rgba(110, 118, 129, 0.35);

  .md-callout--note { --md-callout-accent: #58a6ff; }
  .md-callout--tip { --md-callout-accent: #3fb950; }
  .md-callout--important { --md-callout-accent: #a371f7; --md-callout-bg: rgba(163, 113, 247, 0.12); }
  .md-callout--warning { --md-callout-accent: #d29922; }
  .md-callout--caution { --md-callout-accent: #f85149; }

  .hljs-comment, .hljs-quote { color: #8b949e; }
  .hljs-keyword, .hljs-selector-tag, .hljs-built_in, .hljs-meta .hljs-keyword { color: #ff7b72; }
  .hljs-string, .hljs-regexp, .hljs-addition, .hljs-meta .hljs-string { color: #a5d6ff; }
  .hljs-number, .hljs-literal, .hljs-symbol, .hljs-bullet { color: #79c0ff; }
  .hljs-title, .hljs-title.function_, .hljs-section { color: #d2a8ff; }
  .hljs-attr, .hljs-attribute, .hljs-variable, .hljs-template-variable, .hljs-property { color: #79c0ff; }
  .hljs-name, .hljs-tag, .hljs-selector-class, .hljs-selector-id { color: #7ee787; }
  .hljs-meta, .hljs-params { color: #ffa657; }
  .hljs-deletion { color: #ffdcd7; background: #67060c; }
}

@media print {
  .md-body .md-code__copy, .md-body .md-heading-anchor { display: none; }
}
</style>
