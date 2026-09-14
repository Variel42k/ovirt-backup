import { Dialog } from 'quasar'
import { onBeforeUnmount, onMounted, toValue, type MaybeRefOrGetter } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'

const defaultMessage = 'Введённые изменения ещё не сохранены. Если продолжить, они будут потеряны.'
let pendingConfirmation: Promise<boolean> | null = null

/** Показывает одно общее подтверждение, даже если закрытие вызвано дважды. */
export function confirmDiscardChanges(message = defaultMessage): Promise<boolean> {
  if (pendingConfirmation) return pendingConfirmation

  pendingConfirmation = new Promise<boolean>((resolve) => {
    let settled = false
    const finish = (answer: boolean) => {
      if (settled) return
      settled = true
      pendingConfirmation = null
      resolve(answer)
    }

    Dialog.create({
      title: 'Не сохранять изменения?',
      message,
      persistent: true,
      cancel: { label: 'Остаться', flat: true },
      ok: { label: 'Не сохранять', color: 'negative', unelevated: true },
    })
      .onOk(() => finish(true))
      .onCancel(() => finish(false))
      .onDismiss(() => finish(false))
  })

  return pendingConfirmation
}

/**
 * Защищает несохранённую форму при переходе по маршруту и обновлении вкладки.
 * confirmDiscard используется кнопкой «Отмена», чтобы правило было одинаковым
 * для всех способов уйти из формы.
 */
export function useUnsavedChanges(dirty: MaybeRefOrGetter<boolean>, message = defaultMessage) {
  const isDirty = () => Boolean(toValue(dirty))

  const confirmDiscard = async (): Promise<boolean> => (
    !isDirty() || confirmDiscardChanges(message)
  )

  const beforeUnload = (event: BeforeUnloadEvent) => {
    if (!isDirty()) return
    event.preventDefault()
    event.returnValue = ''
  }

  onMounted(() => window.addEventListener('beforeunload', beforeUnload))
  onBeforeUnmount(() => window.removeEventListener('beforeunload', beforeUnload))
  onBeforeRouteLeave(() => confirmDiscard())

  return { confirmDiscard }
}
