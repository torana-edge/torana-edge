import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {runInNewContext} from 'node:vm';
function setup() {
  const nodes=[];
  const previousFocus={isConnected:true, focus(){this.restored=true;}};
  function createElement(tag) {
    const node={tag, children:[], events:{}, append(...children){this.children.push(...children);},
      appendChild(child){this.children.push(child);}, setAttribute(){}, focus(){},
      querySelector(tag){return this.children.find(child=>child.tag===tag);},
      addEventListener(name, fn){this.events[name]=fn;}, showModal(){this.open=true;},
      close(){this.open=false;this.events.close();}, remove(){this.removed=true;},
      getBoundingClientRect(){return {left:10,top:10,right:200,bottom:200};},
      get childElementCount(){return this.children.length;}};
    nodes.push(node);return node;
  }
  const document={createElement,activeElement:previousFocus,body:createElement('body')};
  const context={document};
  runInNewContext(readFileSync(new URL('./dist/approval.js',import.meta.url),'utf8'),context);
  return {api:context.ToranaApproval,nodes,previousFocus};
}
const review={name:'<img src=x onerror=alert(1)>',digest:'sha256:abc',permissions:['env.file_append'],failureMode:'pass',requirements:'',conflicts:'',resources:{files:{'usage.jsonl':{max_bytes:16777216,retained_files:5}}}};
test('approval requires an explicit click; cancel restores focus and permits reopening',async()=>{
  const app=setup();const result=app.api.confirm(review);
  assert.equal(await app.api.confirm(review),false);
  app.nodes.find(n=>n.textContent==='Cancel').events.click();
  assert.equal(await result,false);
  assert.equal(app.previousFocus.restored,true);
  assert.equal(app.nodes.find(n=>n.tag==='dialog').removed,true);
  const second=app.api.confirm(review);
  app.nodes.filter(n=>n.textContent==='Approve and enable').at(-1).events.click();
  assert.equal(await second,true);
});
test('native Escape/close and backdrop dismissal never grant approval',async()=>{
  for(const backdrop of [false,true]) {
    const app=setup();const result=app.api.confirm(review);const dialog=app.nodes.find(n=>n.tag==='dialog');
    if(backdrop) dialog.events.click({target:dialog,clientX:0,clientY:0}); else dialog.close();
    assert.equal(await result,false);
  }
});
test('untrusted plugin labels are text and exact grant remains available',async()=>{
  const app=setup();const result=app.api.confirm(review);
  assert.equal(app.nodes.some(n=>Object.hasOwn(n,'innerHTML')),false);
  const raw=app.nodes.find(n=>n.tag==='pre').textContent;
  assert.deepEqual(JSON.parse(raw),{digest:review.digest,permissions:review.permissions,...review.resources});
  assert.ok(app.nodes.some(n=>n.textContent?.includes('16,777,216 bytes')));
  app.nodes.find(n=>n.tag==='dialog').close();await result;
});
