// Modifiche a config.json fatte dalla dashboard per conto dell'utente (la
// pagina Config le mostra nel diff prima del salvataggio).

// AddModel: un modello appena costruito da aggiungere a "models"
// (BuildJob → navigate('/config', { state: { addModel } })).
export interface AddModel {
  model_key: string
  tflite_dir: string
}

// withModel restituisce content con m aggiunto a "models", riformattato a 2
// spazi (il diff mostra tutto ciò che cambia), oppure il motivo per cui non
// si può fare in automatico.
export function withModel(content: string, m: AddModel): { text: string } | { reason: string } {
  let cfg: unknown
  try {
    cfg = JSON.parse(content)
  } catch {
    return { reason: 'config.json is not valid JSON' }
  }
  if (typeof cfg !== 'object' || cfg === null || !Array.isArray((cfg as { models?: unknown }).models)) {
    return { reason: 'config.json has no "models" list' }
  }
  const models = (cfg as { models: { model_key?: string }[] }).models
  if (models.some((x) => x.model_key === m.model_key)) {
    return { reason: `"${m.model_key}" is already in "models"` }
  }
  models.push({ model_key: m.model_key, tflite_dir: m.tflite_dir } as { model_key: string })
  return { text: JSON.stringify(cfg, null, 2) + '\n' }
}
