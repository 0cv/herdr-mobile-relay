const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.1-471-a10366df3115fff4/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
