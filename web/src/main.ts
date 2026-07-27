import './styles/tokens.css'
import './styles/app.css'
import { mount } from 'svelte'
import App from './App.svelte'
import { BRAND } from './lib/brand'

// index.html carries the name too, for the moment before the bundle runs; this is
// what actually names the tab, and it is the single source (see lib/brand).
document.title = BRAND

const app = mount(App, { target: document.getElementById('app')! })

export default app
