import { ref } from 'vue'
import { api } from '@/api/client'
import type { DocGuide, DocGuideContent } from '@/api/types'
import { annotateHeadings, escapeHtml, renderMarkdown, setKnownGuides, type DocHeading } from './markdown'

// Руководства: оглавление, тексты и поиск по разделам.
//
// Всё кэшируется на время жизни страницы: тексты встроены в бинарь и между
// запросами не меняются, а повторно тянуть мегабайт Markdown при каждом
// открытии раздела незачем.

export const guideList = ref<DocGuide[]>([])
let listRequest: Promise<DocGuide[]> | null = null
const contentCache = new Map<string, Promise<DocGuideContent>>()

export function loadGuideList(): Promise<DocGuide[]> {
  listRequest ??= api.guides().then((list) => {
    guideList.value = list
    setKnownGuides(list.map((guide) => guide.slug))
    return list
  }).catch((err: unknown) => {
    listRequest = null
    throw err
  })
  return listRequest
}

export function loadGuide(slug: string): Promise<DocGuideContent> {
  let request = contentCache.get(slug)
  if (!request) {
    request = api.guide(slug).catch((err: unknown) => {
      contentCache.delete(slug)
      throw err
    })
    contentCache.set(slug, request)
  }
  return request
}

/** Заголовки выбранных уровней — с теми же id, что получит отрисованная страница. */
export function headingsOf(markdown: string, levels: number[]): DocHeading[] {
  const dom = new DOMParser().parseFromString(`<div id="md-root">${renderMarkdown(markdown)}</div>`, 'text/html')
  const root = dom.getElementById('md-root')
  return root ? annotateHeadings(root, false).filter((h) => levels.includes(h.level)) : []
}

/** Оценка времени чтения: 180 слов в минуту — неспешное чтение технического текста. */
export function readingMinutes(words: number): number {
  return Math.max(1, Math.round(words / 180))
}

/** Руководства по разделам в порядке, который задал сервер. */
export function groupByCategory(list: DocGuide[]): Array<{ category: string; guides: DocGuide[] }> {
  const groups: Array<{ category: string; guides: DocGuide[] }> = []
  for (const guide of list) {
    let group = groups.find((item) => item.category === guide.category)
    if (!group) {
      group = { category: guide.category, guides: [] }
      groups.push(group)
    }
    group.guides.push(guide)
  }
  return groups
}

// Поиск.

/** Один раздел руководства — единица поиска. */
interface SearchSection {
  guide: DocGuide
  /** id заголовка раздела; пусто — начало руководства. */
  headingId: string
  heading: string
  level: number
  text: string
  /** Нормализованные копии для сравнения той же длины, что и оригинал. */
  textKey: string
  headingKey: string
  titleKey: string
  order: number
}

export interface SearchHit {
  guide: DocGuide
  headingId: string
  heading: string
  /** HTML с экранированным текстом и подсвеченными совпадениями. */
  snippet: string
  score: number
  order: number
}

export interface SearchGroup {
  guide: DocGuide
  hits: SearchHit[]
  best: number
}

/**
 * Приведение к виду для сравнения. Длина строки не меняется — на этом держится
 * подсветка совпадений в исходном тексте: «ё» ищется и по «е».
 */
function searchKey(text: string): string {
  return text.toLocaleLowerCase('ru').replace(/ё/g, 'е')
}

let indexRequest: Promise<SearchSection[]> | null = null

/** Индекс строится при первом поиске: до этого тексты всех руководств не нужны. */
export function loadSearchIndex(): Promise<SearchSection[]> {
  indexRequest ??= buildSearchIndex().catch((err: unknown) => {
    indexRequest = null
    throw err
  })
  return indexRequest
}

async function buildSearchIndex(): Promise<SearchSection[]> {
  const list = await loadGuideList()
  const docs = await Promise.all(list.map((guide) => loadGuide(guide.slug)))
  const parser = new DOMParser()
  const sections: SearchSection[] = []
  let order = 0

  docs.forEach((doc, index) => {
    const guide = list[index]
    const dom = parser.parseFromString(`<div id="md-root">${renderMarkdown(doc.markdown)}</div>`, 'text/html')
    const root = dom.getElementById('md-root')
    if (!root) return
    // Подписи блоков кода и схем — не текст руководства: иначе «копировать»
    // находилось бы в каждом разделе с примером.
    root.querySelectorAll('.md-code__bar, .md-diagram__caption').forEach((el) => el.remove())
    annotateHeadings(root, false)

    let current = { headingId: '', heading: guide.title, level: 1, parts: [] as string[] }
    const flush = () => {
      const text = current.parts.join(' ').replace(/\s+/g, ' ').trim()
      if (!text && current.level === 1) return
      sections.push({
        guide, headingId: current.headingId, heading: current.heading, level: current.level,
        text, textKey: searchKey(text), headingKey: searchKey(current.heading),
        titleKey: searchKey(guide.title), order: order++,
      })
    }
    for (const node of Array.from(root.children)) {
      if (/^H[1-6]$/.test(node.tagName)) {
        flush()
        current = {
          headingId: node.id,
          heading: (node.textContent ?? '').trim(),
          level: Number(node.tagName.slice(1)),
          parts: [],
        }
      } else {
        current.parts.push(node.textContent ?? '')
      }
    }
    flush()
  })
  return sections
}

/** Фрагмент текста вокруг первого совпадения с подсветкой всех терминов. */
function snippetOf(text: string, key: string, terms: string[]): string {
  if (!text) return ''
  let first = -1
  for (const term of terms) {
    const at = key.indexOf(term)
    if (at >= 0 && (first < 0 || at < first)) first = at
  }
  const start = first < 0 ? 0 : Math.max(0, first - 80)
  const end = Math.min(text.length, (first < 0 ? 0 : first) + 180)
  const part = text.slice(start, end)
  const partKey = key.slice(start, end)

  // Отмечаем совпадающие позиции, затем собираем HTML, экранируя всё остальное.
  const marked = new Array<boolean>(part.length).fill(false)
  for (const term of terms) {
    let at = partKey.indexOf(term)
    while (at >= 0) {
      for (let i = at; i < at + term.length; i++) marked[i] = true
      at = partKey.indexOf(term, at + term.length)
    }
  }
  let html = start > 0 ? '…' : ''
  let i = 0
  while (i < part.length) {
    const inMark = marked[i]
    let j = i
    while (j < part.length && marked[j] === inMark) j++
    const chunk = escapeHtml(part.slice(i, j))
    html += inMark ? `<mark>${chunk}</mark>` : chunk
    i = j
  }
  return html + (end < text.length ? '…' : '')
}

/**
 * Поиск по разделам руководств.
 *
 * Все слова запроса должны встретиться в разделе (или в названии руководства),
 * но хотя бы одно — в самом разделе: иначе совпадение по названию вытаскивало бы
 * каждый раздел руководства подряд. Заголовок раздела весит больше текста.
 */
export function searchGuides(index: SearchSection[], query: string, limit = 60): SearchGroup[] {
  const terms = searchKey(query).split(/\s+/).map((term) => term.trim()).filter((term) => term.length > 1)
  if (terms.length === 0) return []

  const hits: SearchHit[] = []
  for (const section of index) {
    let score = 0
    let everyTerm = true
    let inSection = false
    for (const term of terms) {
      const inHeading = section.headingKey.includes(term)
      const inText = section.textKey.includes(term)
      const inTitle = section.titleKey.includes(term)
      if (!inHeading && !inText && !inTitle) {
        everyTerm = false
        break
      }
      inSection ||= inHeading || inText
      score += (inHeading ? 10 : 0) + (inTitle ? 3 : 0) + (inText ? 1 : 0)
    }
    if (!everyTerm || !inSection) continue
    hits.push({
      guide: section.guide,
      headingId: section.headingId,
      heading: section.heading,
      snippet: snippetOf(section.text, section.textKey, terms),
      score,
      order: section.order,
    })
  }

  hits.sort((a, b) => b.score - a.score || a.order - b.order)
  const groups = new Map<string, SearchGroup>()
  for (const hit of hits.slice(0, limit)) {
    let group = groups.get(hit.guide.slug)
    if (!group) {
      group = { guide: hit.guide, hits: [], best: hit.score }
      groups.set(hit.guide.slug, group)
    }
    group.hits.push(hit)
  }
  return Array.from(groups.values())
}
