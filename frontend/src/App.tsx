import { useCallback, useEffect, useState } from 'react'
import { Activity, AlertTriangle, CheckCircle2, Cloud, RefreshCw, ShieldCheck } from 'lucide-react'

type Metric = { label:string; value:string; percent?:number; resetAt?:string; rows?:{label:string;value:string}[] }
type Provider = { id:string; name:string; status:'ok'|'no_data'|'error'; configured:boolean; summary:string; message?:string; source:string; metrics:Metric[]; updatedAt:string }
type Dashboard = { providers:Provider[]; updatedAt:string }

function formatReset(value?:string) {
  if (!value) return ''
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  const abs = new Intl.DateTimeFormat('pl-PL',{dateStyle:'short',timeStyle:'short'}).format(d)
  const rel = formatRelative(value)
  return rel ? `${abs} (${rel})` : abs
}

function formatRelative(value:string) {
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return ''
  const diffMs = d.getTime() - Date.now()
  const future = diffMs >= 0
  const totalMin = Math.floor(Math.abs(diffMs) / 60000)
  if (totalMin < 1) return future ? 'in a moment' : 'a moment ago'
  const days = Math.floor(totalMin / 1440)
  const hours = Math.floor((totalMin % 1440) / 60)
  const mins = totalMin % 60
  const parts:string[] = []
  if (days > 0) parts.push(`${days} ${days === 1 ? 'day' : 'days'}`)
  if (hours > 0) parts.push(`${hours} h`)
  if (mins > 0) parts.push(`${mins} min`)
  const body = parts.join(' ')
  return future ? `in ${body}` : `${body} ago`
}

function ProviderCard({p}:{p:Provider}) {
  const icon = p.status === 'ok' ? <CheckCircle2 size={18}/> : p.status === 'error' ? <AlertTriangle size={18}/> : <Cloud size={18}/>
  return <article className={`card ${p.status}`}>
    <header className="cardHead">
      <div><div className="eyebrow">{p.source}</div><h2>{p.name}</h2></div>
      <span className={`badge ${p.status}`}>{icon}{p.status === 'ok' ? 'Data available' : p.status === 'error' ? 'Fetch error' : 'No data'}</span>
    </header>
    <p className="summary">{p.summary}</p>
    {p.metrics.length > 0 ? <div className="metrics">{p.metrics.map((m,i)=><div className="metric" key={`${m.label}-${i}`}>
      <div className="metricLine"><span>{m.label}</span><strong>{m.value}</strong></div>
      {typeof m.percent === 'number' && <div className="track" aria-label={`${m.label}: ${m.percent}%`}><div className="fill" style={{width:`${Math.min(100,Math.max(0,m.percent))}%`}}/></div>}
      {m.resetAt && <div className="reset">Reset: {formatReset(m.resetAt)}</div>}
      {m.rows && m.rows.length > 0 && <ul className="rows">{m.rows.map((row,j)=><li key={`${row.label}-${j}`}><span>{row.label}</span><strong>{row.value}</strong></li>)}</ul>}
    </div>)}</div> : <div className="empty"><Activity size={20}/><span>No metrics to display.</span></div>}
    {p.message && <p className="message">{p.message}</p>}
    <footer><span>{p.configured ? 'Backend configured' : 'ENV not configured'}</span><span>Keys never reach the frontend</span></footer>
  </article>
}

export default function App(){
  const [data,setData] = useState<Dashboard|null>(null)
  const [loading,setLoading] = useState(true)
  const [error,setError] = useState('')
  const load = useCallback(async()=>{
    setLoading(true); setError('')
    try { const r=await fetch('/api/dashboard',{cache:'no-store'}); if(!r.ok) throw new Error(`HTTP ${r.status}`); setData(await r.json()) }
    catch(e){ setError(e instanceof Error?e.message:'Unknown error') }
    finally { setLoading(false) }
  },[])
  useEffect(()=>{void load()},[load])
  return <main className="page">
    <section className="hero">
      <div><div className="kicker"><ShieldCheck size={16}/> Read-only usage monitor</div><h1>AI Usage Dashboard</h1><p>One view of AI subscription limits and API usage. The backend only queries usage/cost endpoints — it never sends prompts to models.</p></div>
      <button onClick={()=>void load()} disabled={loading}><RefreshCw size={17} className={loading?'spin':''}/>{loading?'Refreshing…':'Refresh'}</button>
    </section>
    {error && <div className="globalError">Failed to refresh the dashboard: {error}.{data ? ' Showing the last loaded data.' : ''}</div>}
    {data && data.providers.length === 0 && <div className="globalError">No tiles configured. Set TILES in the backend .env (see .env.example).</div>}
    <section className="grid">{(data?.providers ?? []).map(p=><ProviderCard p={p} key={p.id}/>)}</section>
    <div className="meta">{data?.updatedAt ? `Last refreshed: ${new Date(data.updatedAt).toLocaleString('en-GB')}` : 'Waiting for data…'} · manual refresh</div>
  </main>
}
