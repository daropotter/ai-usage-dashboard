import { useCallback, useEffect, useMemo, useState } from 'react'
import { Activity, AlertTriangle, CheckCircle2, Cloud, RefreshCw, ShieldCheck } from 'lucide-react'

type Metric = { label:string; value:string; percent?:number; resetAt?:string }
type Provider = { id:string; name:string; status:'ok'|'no_data'|'error'; configured:boolean; summary:string; message?:string; source:string; metrics:Metric[]; updatedAt:string }
type Dashboard = { providers:Provider[]; updatedAt:string }

const expected: Provider[] = [
  {id:'openai',name:'OpenAI',status:'no_data',configured:false,summary:'Brak danych',source:'OpenAI',metrics:[],updatedAt:''},
  {id:'anthropic',name:'Claude / Anthropic',status:'no_data',configured:false,summary:'Brak danych',source:'Anthropic',metrics:[],updatedAt:''},
  {id:'opencode-go',name:'OpenCode Go',status:'no_data',configured:false,summary:'Brak danych',source:'OpenCode',metrics:[],updatedAt:''},
  {id:'ollama-cloud',name:'Ollama Cloud',status:'no_data',configured:false,summary:'Brak danych',source:'Ollama',metrics:[],updatedAt:''},
]

function formatReset(value?:string) {
  if (!value) return ''
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return new Intl.DateTimeFormat('pl-PL',{dateStyle:'short',timeStyle:'short'}).format(d)
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
