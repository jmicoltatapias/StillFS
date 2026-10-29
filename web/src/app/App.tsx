import { useState } from 'react'
import { SystemStatus } from '../features/system/SystemStatus'

type Page = 'home' | 'system'

export function App() {
  const [page, setPage] = useState<Page>('home')

  return (
    <div className="shell">
      <aside className="sidebar">
        <a className="brand" href="#home" onClick={() => setPage('home')}>
          <span className="brand-mark">S</span>
          <span>StillFS</span>
        </a>
        <p className="nav-label">ESPACIO DE TRABAJO</p>
        <nav aria-label="Navegación principal">
          <button className={page === 'home' ? 'nav-item active' : 'nav-item'} onClick={() => setPage('home')}>
            <span aria-hidden="true">⌂</span> Inicio
          </button>
          <button className={page === 'system' ? 'nav-item active' : 'nav-item'} onClick={() => setPage('system')}>
            <span aria-hidden="true">◉</span> Estado del sistema
          </button>
        </nav>
        <div className="sidebar-foot">Infraestructura de almacenamiento</div>
      </aside>

      <main className="main">
        <header className="topbar"><span>StillFS / {page === 'home' ? 'Inicio' : 'Sistema'}</span><span className="phase">Fase inicial</span></header>
        <div className="content">
          {page === 'home' ? <Home onOpenStatus={() => setPage('system')} /> : <SystemStatus />}
        </div>
      </main>
    </div>
  )
}

function Home({ onOpenStatus }: { onOpenStatus: () => void }) {
  return (
    <>
      <p className="eyebrow">ALMACENAMIENTO DE OBJETOS</p>
      <h1>Bienvenido a StillFS</h1>
      <p className="intro">La infraestructura inicial está lista para configurarse. Consulta el estado del servicio y de la base de datos.</p>
      <button className="primary" onClick={onOpenStatus}>Ver estado del sistema <span aria-hidden="true">→</span></button>
      <section className="note-card">
        <span className="note-icon" aria-hidden="true">i</span>
        <div><h2>Fase de infraestructura</h2><p>Las operaciones S3 y el almacenamiento de objetos aún no están habilitados.</p></div>
      </section>
    </>
  )
}
