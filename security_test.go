package main

import (
 "encoding/json"
 "net/http/httptest"
 "path/filepath"
 "strings"
 "testing"
)

func TestSiteOriginAndReaderIsolation(t *testing.T) {
 s:=setup(t)
 s.cfg.AdminHost,s.cfg.ContentHost="admin.example.test","pages.example.test"
 paths:=map[string]string{}
 for _,name:=range []string{"private","other"} {
  r:=httptest.NewRequest("POST","/api/publish",strings.NewReader(`{"site":"`+name+`","path":"index.html","content":"secret","visibility":"protected"}`))
  r.Host=s.cfg.AdminHost; r.Header.Set("Authorization","Bearer admin-secret")
  w:=httptest.NewRecorder(); s.ServeHTTP(w,r)
  if w.Code!=201 { t.Fatal(w.Code,w.Body.String()) }
  var out map[string]string
  if err:=json.Unmarshal(w.Body.Bytes(),&out); err!=nil { t.Fatal(err) }
  paths[name]="/p/"+out["slug"]+"/index.html"
  if !strings.Contains(out["url"],out["slug"]+".pages.example.test") { t.Fatal(out) }
 }
 request:=func(host,p string,basic bool)*httptest.ResponseRecorder {
  r:=httptest.NewRequest("GET",p,nil);r.Host=host
  if basic { r.SetBasicAuth("alex","reader-secret") }
  w:=httptest.NewRecorder();s.ServeHTTP(w,r);return w
 }
 first:=strings.Split(paths["private"],"/")[2]+".pages.example.test"
 second:=strings.Split(paths["other"],"/")[2]+".pages.example.test"
 if w:=request(first,paths["private"],true);w.Code!=200||w.Header().Get("Cache-Control")!="private, no-store" {t.Fatal(w.Code,w.Header())}
 if w:=request(first,paths["other"],true);w.Code!=404 {t.Fatal(w.Code)}
 if w:=request(second,paths["other"],true);w.Code!=401 {t.Fatal(w.Code)}
 if w:=request("pages.example.test",paths["private"],true);w.Code!=404 {t.Fatal(w.Code)}
 if w:=request("random.test","/health",false);w.Code!=200 {t.Fatal(w.Code)}
}

func TestRollbackAndLimits(t *testing.T) {
 s:=setup(t)
 post:=func(content string)*httptest.ResponseRecorder {
  b,_:=json.Marshal(publication{Site:"newser",Path:"index.html",Content:content})
  return call(s,"POST","/api/publish","bot-secret",b)
 }
 if w:=post("one");w.Code!=201 {t.Fatal(w.Code,w.Body.String())}
 w:=post("two");if w.Code!=201 {t.Fatal(w.Code,w.Body.String())}
 var out map[string]string;json.Unmarshal(w.Body.Bytes(),&out)
 if w=call(s,"GET","/p/"+out["slug"]+"/index.html.previous","",nil);w.Code!=404 {t.Fatal(w.Code)}
 w=call(s,"POST","/api/rollback","bot-secret",[]byte(`{"site":"newser","path":"index.html"}`))
 if w.Code!=200 {t.Fatal(w.Code,w.Body.String())}
 if w=call(s,"GET","/p/"+out["slug"]+"/index.html","",nil);w.Body.String()!="one" {t.Fatal(w.Code,w.Body.String())}
 for i:=0;i<57;i++ {if w=call(s,"POST","/api/publish","bot-secret",[]byte(`{}`));w.Code==429 {t.Fatal("early limit",i)}}
 if w=post("three");w.Code!=429 {t.Fatal("limit",w.Code)}
 if w=call(s,"POST","/api/publish","admin-secret",[]byte(`{"site":"huge","path":"index.html","content":"x"}`));w.Code!=201 {t.Fatal("independent actor",w.Code)}
 if _,err:=NewServer(Config{Dir:t.TempDir(),AdminToken:"x",AdminHost:"a"});err==nil {t.Fatal("partial host config")}
 if s.withinQuota("newser",filepath.Join(s.cfg.Dir,"newser",out["slug"],"oversize.bin"),maxSiteBytes) {t.Fatal("quota accepted oversized addition")}
}
