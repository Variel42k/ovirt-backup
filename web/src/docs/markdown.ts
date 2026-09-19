import { Marked, Renderer } from 'marked'
import hljs from 'highlight.js/lib/core'
import bash from 'highlight.js/lib/languages/bash'
import dockerfile from 'highlight.js/lib/languages/dockerfile'
import go from 'highlight.js/lib/languages/go'
import http from 'highlight.js/lib/languages/http'
import ini from 'highlight.js/lib/languages/ini'
import javascript from 'highlight.js/lib/languages/javascript'
import json from 'highlight.js/lib/languages/json'
import nginx from 'highlight.js/lib/languages/nginx'
import plaintext from 'highlight.js/lib/languages/plaintext'
import powershell from 'highlight.js/lib/languages/powershell'
import python from 'highlight.js/lib/languages/python'
import sql from 'highlight.js/lib/languages/sql'
import xml from 'highlight.js/lib/languages/xml'
import yaml from 'highlight.js/lib/languages/yaml'

// Отрисовка руководств из docs/.
//
// Тексты свои и собраны в бинарь, но HTML из них всё равно не исполняется:
// сырой HTML в Markdown показывается как текст, ссылки пропускаются только с
// безопасными схемами. Документация не должна быть местом, где однажды
// вставленный фрагмент начнёт выполняться у каждого, кто её открыл.
//
// Подсветка — только нужные языки: полный highlight.js весит на порядок больше.

for (const [name, language] of Object.entries({
  bash, dockerfile, go, http, ini, javascript, json, nginx, plaintext, powershell, python, sql, xml, yaml,
})) {
  hljs.registerLanguage(name, language)
}

/** Как языки из заголовков блоков кода называются в highlight.js. */
const LANGUAGE_ALIASES: Record<string, string> = {
  sh: 'bash', shell: 'bash', console: 'bash', zsh: 'bash',
  jsonc: 'json', dotenv: 'ini', env: 'ini', toml: 'ini', conf: 'ini',
  yml: 'yaml', js: 'javascript', ps1: 'powershell', golang: 'go', html: 'xml',
  text: 'plaintext', txt: 'plaintext',
}

/** Подпись над блоком кода. */
const LANGUAGE_LABELS: Record<string, string> = {
  bash: 'Shell', sh: 'Shell', shell: 'Shell', console: 'Shell',
  yaml: 'YAML', yml: 'YAML', json: 'JSON', jsonc: 'JSON', ini: 'INI', dotenv: '.env', env: '.env',
  powershell: 'PowerShell', nginx: 'nginx', python: 'Python', javascript: 'JavaScript',
  http: 'HTTP', go: 'Go', sql: 'SQL', dockerfile: 'Dockerfile', xml: 'XML', html: 'HTML',
  text: 'Текст', plaintext: 'Текст', txt: 'Текст',
}

const ALERTS: Record<string, { title: string; icon: string }> = {
  note: { title: 'Заметка', icon: 'info' },
  tip: { title: 'Совет', icon: 'lightbulb' },
  important: { title: 'Важно', icon: 'priority_high' },
  warning: { title: 'Внимание', icon: 'warning' },
  caution: { title: 'Осторожно', icon: 'report' },
}

const HTML_ESCAPES: Record<string, string> = {
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
}

export function escapeHtml(text: string): string {
  return text.replace(/[&<>"']/g, (c) => HTML_ESCAPES[c])
}

function safeDecode(value: string): string {
  try {
    return decodeURIComponent(value)
  } catch {
    return value
  }
}

/** Куда ведёт ссылка из руководства. */
export type DocLink =
  | { kind: 'guide'; slug: string; anchor?: string }
  | { kind: 'anchor'; anchor: string }
  | { kind: 'external'; href: string }
  | { kind: 'source'; path: string }
  | { kind: 'text' }

/** ROLES.md, ./ROLES.md, docs/ROLES.md#раздел — другие руководства. */
const GUIDE_LINK = /^(?:\.\/)?(?:docs\/)?([A-Za-z0-9][A-Za-z0-9_-]*)\.md(?:#(.*))?$/

export function resolveDocLink(href: string): DocLink {
  const target = href.trim()
  if (target.startsWith('#')) return { kind: 'anchor', anchor: safeDecode(target.slice(1)) }
  const guide = GUIDE_LINK.exec(target)
  if (guide) {
    return { kind: 'guide', slug: guide[1].toLowerCase(), anchor: guide[2] ? safeDecode(guide[2]) : undefined }
  }
  if (/^(https?:|mailto:)/i.test(target)) return { kind: 'external', href: target }
  // javascript:, data: и прочие схемы — только текст.
  if (/^[a-z][a-z0-9+.-]*:/i.test(target)) return { kind: 'text' }
  // Относительный путь к исходникам (../internal/…): в интерфейсе его не открыть.
  return { kind: 'source', path: target }
}

/** Адрес руководства в интерфейсе. */
export function guideHref(slug: string, anchor?: string): string {
  return `/documentation/${encodeURIComponent(slug)}${anchor ? `#${encodeURIComponent(anchor)}` : ''}`
}

let knownGuides = new Set<string>()

/** Ссылки на руководства, которых нет в сборке, показываются текстом, а не ведут в пустоту. */
export function setKnownGuides(slugs: Iterable<string>): void {
  knownGuides = new Set(slugs)
}

const markdown = new Marked({
  gfm: true,
  breaks: false,
  renderer: {
    html({ text }) {
      return escapeHtml(text)
    },

    code({ text, lang }) {
      const raw = (lang ?? '').trim().split(/\s+/)[0]?.toLowerCase() ?? ''
      if (raw === 'mermaid') {
        // Сам Mermaid весит мегабайты и тянет десятки пакетов в состав сборки.
        // Исходник этих схем — читаемый текст, его и показываем.
        return '<figure class="md-diagram">' +
          '<figcaption class="md-diagram__caption">' +
          '<span class="material-icons" aria-hidden="true">schema</span>Схема · исходник Mermaid</figcaption>' +
          `<pre><code>${escapeHtml(text)}</code></pre></figure>`
      }
      const language = LANGUAGE_ALIASES[raw] ?? raw
      const body = language && hljs.getLanguage(language)
        ? hljs.highlight(text, { language, ignoreIllegals: true }).value
        : escapeHtml(text)
      const label = LANGUAGE_LABELS[raw] ?? (raw || 'Текст')
      return '<div class="md-code">' +
        '<div class="md-code__bar">' +
        `<span class="md-code__lang">${escapeHtml(label)}</span>` +
        '<button type="button" class="md-code__copy" data-md-copy>' +
        '<span class="material-icons" aria-hidden="true">content_copy</span>' +
        '<span class="md-code__copy-label">Копировать</span></button>' +
        '</div>' +
        `<pre><code class="hljs">${body}</code></pre></div>`
    },

    blockquote({ tokens }) {
      const body = this.parser.parse(tokens)
      // Синтаксис предупреждений GitHub: «> [!WARNING]» в начале цитаты.
      const alert = /^<p>\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*/i.exec(body)
      if (alert) {
        const kind = alert[1].toLowerCase()
        const meta = ALERTS[kind]
        return `<div class="md-callout md-callout--${kind}" role="note">` +
          '<div class="md-callout__title">' +
          `<span class="material-icons" aria-hidden="true">${meta.icon}</span>${meta.title}</div>` +
          `<p>${body.slice(alert[0].length)}</div>`
      }
      return `<blockquote class="md-quote">${body}</blockquote>`
    },

    table(token) {
      return `<div class="md-table">${Renderer.prototype.table.call(this, token)}</div>`
    },

    link({ href, title, tokens }) {
      const text = this.parser.parseInline(tokens)
      const titleAttr = title ? ` title="${escapeHtml(title)}"` : ''
      const target = resolveDocLink(href)
      switch (target.kind) {
        case 'guide':
          if (!knownGuides.has(target.slug)) {
            return `<span class="md-srcref" title="Этот файл не входит во встроенную документацию">${text}</span>`
          }
          return `<a href="${escapeHtml(guideHref(target.slug, target.anchor))}"` +
            ` data-md-guide="${escapeHtml(target.slug)}"` +
            (target.anchor ? ` data-md-target="${escapeHtml(target.anchor)}"` : '') +
            `${titleAttr}>${text}</a>`
        case 'anchor':
          return `<a href="#${escapeHtml(encodeURIComponent(target.anchor))}"` +
            ` data-md-target="${escapeHtml(target.anchor)}"${titleAttr}>${text}</a>`
        case 'external':
          return `<a href="${escapeHtml(target.href)}" target="_blank" rel="noopener noreferrer"${titleAttr}>` +
            `${text}<span class="material-icons md-external" aria-hidden="true">open_in_new</span></a>`
        case 'source':
          return `<span class="md-srcref" title="Файл в репозитории: ${escapeHtml(target.path)}">${text}</span>`
        default:
          return text
      }
    },

    image({ text }) {
      return escapeHtml(text)
    },
  },
})

/** Markdown руководства в HTML для v-html. */
export function renderMarkdown(source: string): string {
  return markdown.parse(source, { async: false }) as string
}

/**
 * Идентификатор заголовка по правилам GitHub.
 *
 * Руководства ссылаются друг на друга якорями, которые сгенерировал GitHub:
 * TROUBLESHOOTING.md#5-авторизация. Чтобы эти ссылки работали и здесь, id
 * заголовков считаются тем же алгоритмом, что у github-slugger: нижний
 * регистр, всё, кроме букв, цифр, «_», «-» и пробела, выбрасывается, пробел
 * становится дефисом, повторы получают суффикс -1, -2…
 */
export function githubSlug(text: string): string {
  return text.toLowerCase().replace(/[^\p{L}\p{M}\p{N}\p{Pc}\- ]/gu, '').replace(/ /g, '-')
}

function createSlugger(): (text: string) => string {
  const occurrences = new Map<string, number>()
  return (text: string) => {
    const base = githubSlug(text.trim()) || 'section'
    let slug = base
    while (occurrences.has(slug)) {
      const next = (occurrences.get(base) ?? 0) + 1
      occurrences.set(base, next)
      slug = `${base}-${next}`
    }
    occurrences.set(slug, 0)
    return slug
  }
}

export interface DocHeading {
  id: string
  text: string
  level: number
}

/**
 * Проставляет заголовкам id и собирает их список.
 *
 * Одна функция и для показа, и для поискового индекса: если бы якоря считались
 * в двух местах, результат поиска однажды вёл бы не туда.
 */
export function annotateHeadings(root: ParentNode, withAnchors: boolean): DocHeading[] {
  const slug = createSlugger()
  const headings: DocHeading[] = []
  root.querySelectorAll<HTMLElement>('h1, h2, h3, h4, h5, h6').forEach((el) => {
    const text = (el.textContent ?? '').trim()
    const id = slug(text)
    const level = Number(el.tagName.slice(1))
    el.id = id
    headings.push({ id, text, level })
    if (withAnchors && level > 1) {
      const anchor = el.ownerDocument.createElement('a')
      anchor.className = 'md-heading-anchor'
      anchor.href = `#${encodeURIComponent(id)}`
      anchor.setAttribute('data-md-heading', id)
      anchor.setAttribute('aria-label', 'Скопировать ссылку на раздел')
      anchor.setAttribute('title', 'Скопировать ссылку на раздел')
      anchor.textContent = '#'
      el.appendChild(anchor)
    }
  })
  return headings
}
