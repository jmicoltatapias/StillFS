export type Readiness = {
  status: 'ok' | 'not_ready'
  database?: 'ok' | 'unavailable'
}

export async function fetchReadiness(): Promise<Readiness> {
  const response = await fetch('/health/ready', { headers: { Accept: 'application/json' } })
  if (!response.ok && response.status !== 503) {
    throw new Error(`Unexpected health response: ${response.status}`)
  }
  return response.json() as Promise<Readiness>
}
