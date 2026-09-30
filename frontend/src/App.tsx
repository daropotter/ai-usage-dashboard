import { useCallback, useEffect, useMemo, useState } from 'react'
import { Activity, AlertTriangle, CheckCircle2, Cloud, RefreshCw, ShieldCheck } from 'lucide-react'

type Metric = { label:string; value:string; percent?:number; resetAt?:string; rows?:{label:string;value:string}[] }
type Provider = { id:string; name:string; status:'ok'|'no_data'|'error'; configured:boolean; summary:string; message?:string; source:string; metrics:Metric[]; updatedAt:string }
type Dashboard = { providers:Provider[]; updatedAt:string }

const expected: Provider[] = [
  {id:'openai',name:'OpenAI',status:'no_data',configured:false,summary:'Brak danych',source:'OpenAI',metrics:[],updatedAt:''},
  {id:'chatgpt',name:'ChatGPT (subskrypcja)',status:'no_data',configured:false,summary:'Brak danych',source:'ChatGPT',metrics:[],updatedAt:''},
  {id:'anthropic',name:'Claude / Anthropic',status:'no_data',configured:false,summary:'Brak danych',source:'Anthropic',metrics:[],updatedAt:''},
  {id:'ollama-cloud',name:'Ollama Cloud',status:'no_data',configured:false,summary:'Brak danych',source:'Ollama',metrics:[],updatedAt:''},
  {id:'opencode-go',name:'OpenCode Go A',status:'no_data',configured:false,summary:'Brak danych',source:'OpenCode',metrics:[],updatedAt:''},
  {id:'opencode-go-b',name:'OpenCode Go B',status:'no_data',configured:false,summary:'Brak danych',source:'OpenCode',metrics:[],updatedAt:''},
]

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
  if (totalMin < 1) return future ? 'za chwilę' : 'przed chwilą'
  const days = Math.floor(totalMin / 1440)
  const hours = Math.floor((totalMin % 1440) / 60)
  const mins = totalMin % 60
  const parts:string[] = []
  if (days > 0) parts.push(`${days} ${days === 1 ? 'dzień' : 'dni'}`)
  if (hours > 0) parts.push(`${hours} godz.`)
  if (mins > 0) parts.push(`${mins} min`)
  const body = parts.join(' ')
  return future ? `za ${body}` : `${body} temu`
}

function ProviderCard({p}:{p:Provider}) {
  const icon = p.status === 'ok' ? <CheckCircle2 size={18}/> : p.status === 'error' ? <AlertTriangle size={18}/> : <Cloud size={18}/>
  return <article className={`card ${p.status}`}>
    <header className="cardHead">
      <div><div className="eyebrow">{p.source}</div><h2>{p.name}</h2></div>
      <span className={`badge ${p.status}`}>{icon}{p.status === 'ok' ? 'Dane dostępne' : p.status === 'error' ? 'Błąd pobierania' : 'Brak danych'}</span>
    </header>
    <p className="summary">{p.summary}</p>
    {p.metrics.length > 0 ? <div className="metrics">{p.metrics.map((m,i)=><div className="metric" key={`${m.label}-${i}`}>
      <div className="metricLine"><span>{m.label}</span><strong>{m.value}</strong></div>
      {typeof m.percent === 'number' && <div className="track" aria-label={`${m.label}: ${m.percent}%`}><div className="fill" style={{width:`${Math.min(100,Math.max(0,m.percent))}%`}}/></div>}
      {m.resetAt && <div className="reset">Reset: {formatReset(m.resetAt)}</div>}
      {m.rows && m.rows.length > 0 && <ul className="rows">{m.rows.map((row,j)=><li key={`${row.label}-${j}`}><span>{row.label}</span><strong>{row.value}</strong></li>)}</ul>}
    </div>)}</div> : <div className="empty"><Activity size={20}/><span>Brak metryk do wyświetlenia.</span></div>}
    {p.message && <p className="message">{p.message}</p>}
    <footer><span>{p.configured ? 'Backend skonfigurowany' : 'Brak konfiguracji ENV'}</span><span>Klucze nigdy nie trafiają do frontendu</span></footer>
  </article>
}

export default function App(){
  const [data,setData] = useState<Dashboard|null>(null)
  const [loading,setLoading] = useState(true)
  const [error,setError] = useState('')
  const load = useCallback(async()=>{
    setLoading(true); setError('')
    try { const r=await fetch('/api/dashboard',{cache:'no-store'}); if(!r.ok) throw new Error(`HTTP ${r.status}`); setData(await r.json()) }
    catch(e){ setError(e instanceof Error?e.message:'Nieznany błąd') }
    finally { setLoading(false) }
  },[])
  useEffect(()=>{void load(); const id=setInterval(()=>void load(),60_000); return()=>clearInterval(id)},[load])
  const providers=useMemo(()=>expected.map(f=>data?.providers.find(p=>p.id===f.id)??f),[data])
  return <main className="page">
    <section className="hero">
      <div><div className="kicker"><ShieldCheck size={16}/> Read-only usage monitor</div><h1>AI Usage Dashboard</h1><p>Jeden widok dla OpenAI, Claude, OpenCode Go i Ollama Cloud. Backend odpytuje tylko endpointy usage/cost — nie wysyła promptów do modeli.</p></div>
      <button onClick={()=>void load()} disabled={loading}><RefreshCw size={17} className={loading?'spin':''}/>{loading?'Odświeżanie…':'Odśwież'}</button>
    </section>
    {error && <div className="globalError">Nie udało się odświeżyć dashboardu: {error}. Kafelki pozostają widoczne.</div>}
    <section className="grid">{providers.map(p=><ProviderCard p={p} key={p.id}/>)}</section>
    <div className="meta">{data?.updatedAt ? `Ostatnie odświeżenie: ${new Date(data.updatedAt).toLocaleString('pl-PL')}` : 'Oczekiwanie na dane…'} · automatyczne odświeżanie co 60 s</div>
  </main>
}
