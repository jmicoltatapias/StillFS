import { useEffect, useState } from 'react'
import { fetchReadiness, type Readiness } from '../../shared/api/health'

export function SystemStatus() {
  const [state, setState] = useState<{ data?: Readiness; error?: string }>({})

  useEffect(() => {
    let active = true
    fetchReadiness().then(
      (data) => active && setState({ data }),
      () => active && setState({ error: 'No se pudo consultar el servicio. Comprueba que StillFS esté en ejecución.' }),
    )
    return () => { active = false }
  }, [])

  const ready = state.data?.status === 'ok'
  return (
    <>
      <p className="eyebrow">OBSERVABILIDAD</p>
      <h1>Estado del sistema</h1>
      <p className="intro">Disponibilidad actual del servicio y su conexión a PostgreSQL.</p>
      <section className="status-card" aria-live="polite">
        <div className="status-heading"><div><h2>Servicio StillFS</h2><p>Readiness del proceso y base de datos</p></div>
          <span className={state.error || (state.data && !ready) ? 'badge danger' : ready ? 'badge good' : 'badge pending'}><i />{state.error ? 'Sin conexión' : state.data ? ready ? 'Disponible' : 'No disponible' : 'Consultando'}</span>
        </div>
        <div className="status-row"><span>Proceso HTTP</span><strong>{state.error ? 'No disponible' : state.data ? 'En línea' : 'Consultando'}</strong></div>
        <div className="status-row"><span>PostgreSQL</span><strong>{state.error ? 'Sin confirmar' : state.data?.database === 'ok' ? 'Conectado' : state.data ? 'No disponible' : 'Consultando'}</strong></div>
        {state.error && <p className="error-message">{state.error}</p>}
      </section>
      <p className="status-footnote">La comprobación de readiness consulta la conexión real a PostgreSQL.</p>
    </>
  )
}
