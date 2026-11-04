import { expect,test } from "@playwright/test";
import { mockFleet, work } from "./fixture";
const recovery="**/api/hosts/dev/recovery**";
const health="**/api/hosts/dev/runtime/health";
const plan={runtimeId:"r".repeat(32),instanceId:"i".repeat(32),fingerprint:"f".repeat(64),entries:[{source:work,mode:"reopen",status:"planned"},{source:{...work,id:"agent",title:"Provider conversation",harness:"claude"},mode:"resume",status:"planned"}],blockers:[] as string[]};

test("legacy runtimes cannot restart through an unsupported recovery workflow",async({page})=>{
 await mockFleet(page);await page.route(health,route=>route.fulfill({json:{capabilities:{}}}));
 await page.goto("/#host/dev");const region=page.getByRole("region",{name:"Runtime restart and restore"});await expect(region).toContainText("first upgrade");await expect(region.getByRole("button",{name:"Review restart plan"})).toBeDisabled();
});

test("restart requires reviewing exact shell and provider restoration then explicit confirmation",async({page})=>{
 const fleet=await mockFleet(page);await page.route(health,route=>route.fulfill({json:{capabilities:{runtimeRecoveryV1:true}}}));let job:object|undefined;const starts:any[]=[];
 await page.route(recovery,route=>{const req=route.request();if(req.url().endsWith("/preflight"))return route.fulfill({json:plan});if(req.method()==="GET")return route.fulfill({json:{job}});const body=req.postDataJSON();starts.push(body);job={id:body.id,status:"accepted",stage:"Restart accepted"};return route.fulfill({json:job,status:202});});
 await page.goto("/#host/dev");await page.getByRole("button",{name:"Review restart plan"}).click();const dialog=page.getByRole("dialog",{name:"Review runtime restart"});await expect(dialog).toContainText("New shell");await expect(dialog).toContainText("Saved conversation");await expect(dialog.getByRole("button",{name:"Restart and restore",exact:true})).toBeDisabled();
 await dialog.getByRole("checkbox").check();await dialog.getByRole("button",{name:"Restart and restore",exact:true}).click();await expect(dialog).toHaveCount(0);expect(starts).toHaveLength(1);expect(starts[0]).toMatchObject({fingerprint:plan.fingerprint});expect(starts[0].id).toMatch(/^[a-f0-9]{32}$/);expect(fleet.terminalMessages.filter(message=>message.type==="input")).toEqual([]);
});

test("a missing proven provider identity blocks runtime restart",async({page})=>{
 await mockFleet(page);await page.route(health,route=>route.fulfill({json:{capabilities:{runtimeRecoveryV1:true}}}));let starts=0;await page.route(recovery,route=>{if(route.request().url().endsWith("/preflight"))return route.fulfill({json:{...plan,blockers:["Claude Code has no verified conversation ID. Finish this session first."]}});if(route.request().method()!=="GET")starts++;return route.fulfill({json:{}});});
 await page.goto("/#host/dev");await page.getByRole("button",{name:"Review restart plan"}).click();const dialog=page.getByRole("dialog",{name:"Review runtime restart"});await expect(dialog.getByRole("alert")).toContainText("no verified conversation ID");await expect(dialog.getByRole("checkbox")).toBeDisabled();expect(starts).toBe(0);
});

test("unknown restart stays locked for unrelated jobs and reconciles only its accepted identity",async({page})=>{
 await mockFleet(page);await page.route(health,route=>route.fulfill({json:{capabilities:{runtimeRecoveryV1:true}}}));let submitted="",posts=0,match=false;
 await page.route(recovery,route=>{const req=route.request();if(req.url().endsWith("/preflight"))return route.fulfill({json:plan});if(req.method()==="GET")return route.fulfill({json:submitted?{job:{id:match?submitted:"b".repeat(32),status:match?"accepted":"failed",stage:match?"Matching recovery accepted":"Older failed recovery"}}:{}});submitted=req.postDataJSON().id;posts++;return route.abort("failed");});
 await page.goto("/#host/dev");await page.getByRole("button",{name:"Review restart plan"}).click();const dialog=page.getByRole("dialog",{name:"Review runtime restart"});await dialog.getByRole("checkbox").check();await dialog.getByRole("button",{name:"Restart and restore",exact:true}).click();await expect(dialog.getByRole("status")).toContainText("outcome is unknown");await expect(dialog.getByRole("button",{name:"Restart and restore",exact:true})).toBeDisabled();
 await expect(page.getByRole("region",{name:"Runtime restart and restore"})).toContainText("Older failed recovery",{timeout:10000});await expect(dialog).toBeVisible();expect(posts).toBe(1);
 match=true;await expect(dialog).toHaveCount(0,{timeout:10000});await expect(page.getByRole("region",{name:"Runtime restart and restore"})).toContainText("Matching recovery accepted");expect(posts).toBe(1);
});

test("restored receipts open the new exact session and review never restarts again",async({page})=>{
 await mockFleet(page);await page.route(health,route=>route.fulfill({json:{capabilities:{runtimeRecoveryV1:true}}}));let reviews=0;const newSession={...work,id:"restored",createdAt:"2026-11-04T12:00:00Z"};
 await page.route(recovery,route=>{if(route.request().method()==="POST"){expect(route.request().url()).toContain("/review");reviews++;return route.fulfill({json:{}});}return route.fulfill({json:{job:{id:"a".repeat(32),status:"completed",stage:"Restoration completed"},operation:{id:"a".repeat(32),runtimeId:plan.runtimeId,status:"completed",entries:[{source:work,mode:"reopen",status:"restored",session:newSession}]}}});});
 await page.goto("/#host/dev");const region=page.getByRole("region",{name:"Runtime restart and restore"});await expect(region).toContainText("no in-flight command or input was replayed");await region.getByRole("button",{name:"Mark recovery reviewed"}).click();expect(reviews).toBe(1);await region.getByRole("button",{name:"Open restored terminal"}).click();await expect(page).toHaveURL(/session\/restored/);
});
