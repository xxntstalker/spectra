package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMigrationsRunTwiceWithoutHarm(t *testing.T) {
	s := newStore(t)
	// Open again against the same schema: a second replica starting up must
	// find every migration already applied and change nothing.
	if err := s.migrate(context.Background()); err != nil {
		t.Fatalf("повторная миграция: %v", err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// The sweep owns addresses; a person owns names, groups, tags and the manual
// spec URL. Losing an operator's edit on every cluster sync would make the
// portal unusable within a day.
func TestSyncKeepsHumanEdits(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "orders")

	name := "Заказы"
	tags := []string{"core"}
	manual := "http://orders.ns:80/v3/api-docs"
	hidden := ReviewIgnored
	groupID, err := s.CreateGroup(ctx, "Коммерция", 1)
	if err != nil {
		t.Fatal(err)
	}
	group := &groupID
	if err := s.UpdateService(ctx, ids["orders"], ServiceUpdate{
		DisplayName: &name, Tags: &tags, ManualSpecURL: &manual,
		ReviewState: &hidden, GroupID: &group,
	}); err != nil {
		t.Fatal(err)
	}

	// The cluster reports the service again, with the machine-generated name.
	seed(t, s, "orders")

	svc, err := s.GetService(ctx, ids["orders"])
	if err != nil || svc == nil {
		t.Fatalf("сервис пропал: %v", err)
	}
	if svc.DisplayName != name {
		t.Errorf("имя перезаписано синком: %q", svc.DisplayName)
	}
	if svc.ReviewState != ReviewIgnored {
		t.Errorf("состояние «спрятан» сброшено: %q", svc.ReviewState)
	}
	if svc.ManualSpecURL != manual {
		t.Errorf("ручной адрес спеки потерян: %q", svc.ManualSpecURL)
	}
	if len(svc.Tags) != 1 || svc.GroupName != "Коммерция" {
		t.Errorf("теги или группа потеряны: %+v", svc)
	}
}

func TestSyncMarksAbsentAndNamesThem(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seed(t, s, "orders", "billing")

	res, err := s.SyncDiscovered(ctx, "kubernetes", []Discovered{
		{Key: "orders.ns:80", DisplayName: "orders", BaseURL: "http://orders.ns:80"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Gone != 1 || len(res.GoneNames) != 1 || res.GoneNames[0] != "billing" {
		t.Errorf("исчезнувший сервис не назван: %+v", res)
	}

	// Nothing is deleted: history and edits survive a service briefly vanishing.
	services, _ := s.ListServices(ctx)
	if len(services) != 2 {
		t.Errorf("сервис удалён вместо пометки отсутствия: %d", len(services))
	}
}

// A sweep from Kubernetes must not touch entries seeded from the environment.
func TestSyncSourcesAreIndependent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.SyncDiscovered(ctx, "static", []Discovered{
		{Key: "static:extern:80", DisplayName: "extern", BaseURL: "http://extern:80"},
	}); err != nil {
		t.Fatal(err)
	}
	seed(t, s, "orders")

	services, _ := s.ListServices(ctx)
	for _, svc := range services {
		if svc.DisplayName == "extern" && !svc.Present {
			t.Error("синк из кластера пометил отсутствующим сервис из другого источника")
		}
	}
}

func TestAnnotationsFillGapsButNeverOverride(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "orders")

	// Human sets the address by hand first.
	manual := "http://orders.ns:80/manual.json"
	if err := s.UpdateService(ctx, ids["orders"], ServiceUpdate{ManualSpecURL: &manual}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SyncDiscovered(ctx, "kubernetes", []Discovered{{
		Key: "orders.ns:80", DisplayName: "orders", BaseURL: "http://orders.ns:80",
		SpecURL: "http://orders.ns:80/from-annotation.json", Group: "Из аннотации",
	}}); err != nil {
		t.Fatal(err)
	}

	svc, _ := s.GetService(ctx, ids["orders"])
	if svc.SpecURL != manual {
		t.Errorf("аннотация перезаписала ручной адрес: %q", svc.SpecURL)
	}
	// The group was empty, so the annotation may fill it.
	if svc.GroupName != "Из аннотации" {
		t.Errorf("группа из аннотации не проставилась: %q", svc.GroupName)
	}
}

func TestRevisionsAreNumberedPerService(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "orders", "billing")

	for i := 0; i < 3; i++ {
		for _, name := range []string{"orders", "billing"} {
			if _, err := s.InsertVersion(ctx, &Version{
				ServiceID: ids[name], Hash: name + string(rune('a'+i)),
				CapturedAt: time.Now(), Spec: []byte(`{}`),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	for _, name := range []string{"orders", "billing"} {
		versions, err := s.ListVersions(ctx, ids[name], 10)
		if err != nil {
			t.Fatal(err)
		}
		got := []int{}
		for _, v := range versions {
			got = append(got, v.Revision)
		}
		// Newest first: 3, 2, 1 — and independent of the other service.
		if len(got) != 3 || got[0] != 3 || got[2] != 1 {
			t.Errorf("%s: номера ревизий %v, ожидались 3,2,1", name, got)
		}
	}
}

func TestRollbackIsRecognisedByHash(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "orders")

	for _, hash := range []string{"h1", "h2", "h1"} {
		rollback, err := s.PreviousRevisionWithHash(ctx, ids["orders"], hash)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertVersion(ctx, &Version{
			ServiceID: ids["orders"], Hash: hash, CapturedAt: time.Now(),
			Spec: []byte(`{}`), RollbackOf: rollback,
		}); err != nil {
			t.Fatal(err)
		}
	}

	versions, _ := s.ListVersions(ctx, ids["orders"], 10)
	if len(versions) != 3 {
		t.Fatalf("ревизий %d, ожидалось 3 — откат должен создавать запись", len(versions))
	}
	if versions[0].RollbackOf == nil || *versions[0].RollbackOf != 1 {
		t.Errorf("ревизия 3 не помечена откатом к 1: %+v", versions[0].RollbackOf)
	}
	if versions[1].RollbackOf != nil {
		t.Error("обычное изменение помечено откатом")
	}
}

func TestGetVersionUsesTheNumberPeopleSee(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "orders", "billing")

	// Give billing a head start so its primary keys differ from its revisions:
	// looking a revision up by primary key would silently return the wrong one.
	for i := 0; i < 4; i++ {
		s.InsertVersion(ctx, &Version{ServiceID: ids["billing"], Hash: string(rune('a' + i)),
			CapturedAt: time.Now(), Spec: []byte(`{"who":"billing"}`)})
	}
	s.InsertVersion(ctx, &Version{ServiceID: ids["orders"], Hash: "o1",
		CapturedAt: time.Now(), Spec: []byte(`{"who":"orders"}`)})

	v, err := s.GetVersion(ctx, ids["orders"], 1)
	if err != nil || v == nil {
		t.Fatalf("ревизия 1 сервиса orders не найдена: %v", err)
	}
	if string(v.Spec) != `{"who":"orders"}` {
		t.Errorf("вернулась чужая ревизия: %s", v.Spec)
	}
	if v, _ := s.GetVersion(ctx, ids["orders"], 99); v != nil {
		t.Error("несуществующая ревизия вернула данные")
	}
}

func TestPruneKeepsNewestAndDropsOrphanedChanges(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "orders", "billing")

	for i := 0; i < 8; i++ {
		id, err := s.InsertVersion(ctx, &Version{ServiceID: ids["orders"],
			Hash: string(rune('a' + i)), CapturedAt: time.Now(), Spec: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.InsertChange(ctx, &Change{ServiceID: ids["orders"],
			ToVersionID: id, DetectedAt: time.Now(), Diff: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	other, _ := s.InsertVersion(ctx, &Version{ServiceID: ids["billing"],
		Hash: "z", CapturedAt: time.Now(), Spec: []byte(`{}`)})

	removed, err := s.PruneVersions(ctx, ids["orders"], 3)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 5 {
		t.Errorf("удалено %d ревизий, ожидалось 5", removed)
	}
	if versions, _ := s.ListVersions(ctx, ids["orders"], 100); len(versions) != 3 {
		t.Errorf("осталось %d ревизий", len(versions))
	}
	if changes, _ := s.ListChanges(ctx, ids["orders"], 100); len(changes) != 3 {
		t.Errorf("осталось %d записей в ленте — запись без ревизии не отрисуется", len(changes))
	}
	if v, _ := s.GetVersion(ctx, ids["billing"], 1); v == nil || v.ID != other {
		t.Error("подчистка одного сервиса задела другой")
	}
}

// The probe writes carry a CASE over a bool; a driver binding it differently
// would silently stop recording successes.
func TestRecordProbeTracksStateAndBackoff(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "orders")
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.RecordProbe(ctx, ProbeResult{ServiceID: ids["orders"], Status: ProbeOK,
		Succeeded: true, AutoProbe: true, CheckedAt: now,
		SpecURL: "http://orders.ns:80/openapi.json", SpecURLSource: SourceProbe,
		ETag: `"v1"`}); err != nil {
		t.Fatal(err)
	}
	svc, _ := s.GetService(ctx, ids["orders"])
	if svc.LastSuccessAt == nil || !svc.LastSuccessAt.Equal(now) {
		t.Fatalf("успешный опрос не записан: %v", svc.LastSuccessAt)
	}
	if svc.ETag != `"v1"` {
		t.Errorf("валидатор не сохранён: %q", svc.ETag)
	}

	// A service that answered without a spec leaves the schedule entirely.
	later := now.Add(time.Minute)
	if err := s.RecordProbe(ctx, ProbeResult{ServiceID: ids["orders"], Status: ProbeNoSpec,
		Error: "нет спеки", CheckedAt: later, AutoProbe: false}); err != nil {
		t.Fatal(err)
	}
	svc, _ = s.GetService(ctx, ids["orders"])
	if svc.AutoProbe {
		t.Error("автоопрос не выключился после отсутствия спеки")
	}
	if svc.LastSuccessAt == nil || !svc.LastSuccessAt.Equal(now) {
		t.Error("неудача затёрла время последнего успеха")
	}
	if svc.FailCount != 1 {
		t.Errorf("счётчик неудач = %d", svc.FailCount)
	}

	if due, _ := s.PollCandidates(ctx, time.Now()); len(due) != 0 {
		t.Error("сервис без спеки остался в очереди автоматического опроса")
	}

	// A person pressing "probe now" must get one whatever happened before.
	if err := s.ClearBackoff(ctx, ids["orders"]); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.PollCandidates(ctx, time.Now()); len(due) != 1 {
		t.Error("ручной запрос не вернул сервис в очередь")
	}
}

func TestPollCandidatesRespectHiddenAndBackoff(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "visible", "hidden", "waiting")

	ignored := ReviewIgnored
	if err := s.UpdateService(ctx, ids["hidden"], ServiceUpdate{ReviewState: &ignored}); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := s.RecordProbe(ctx, ProbeResult{ServiceID: ids["waiting"],
		Status: ProbeUnreachable, CheckedAt: time.Now(), AutoProbe: true,
		NextProbeAt: &future}); err != nil {
		t.Fatal(err)
	}

	due, err := s.PollCandidates(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].DisplayName != "visible" {
		names := []string{}
		for _, d := range due {
			names = append(names, d.DisplayName)
		}
		t.Errorf("к опросу отобраны %v, ожидался только visible", names)
	}
}

func TestSettingsRoundTripAndSurviveBadRows(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	want := DefaultSettings()
	want.PollInterval = 30 * time.Minute
	want.IgnoreFields = []string{"info.version", "info.description"}
	want.Concurrency = 4
	if err := s.SaveSettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.PollInterval != want.PollInterval || got.Concurrency != 4 || len(got.IgnoreFields) != 2 {
		t.Errorf("настройки не сохранились: %+v", got)
	}

	// One unreadable row must not take the portal down.
	if _, err := s.pool.Exec(ctx,
		`UPDATE settings SET value = 'не длительность' WHERE key = 'poll_interval'`); err != nil {
		t.Fatal(err)
	}
	got, err = s.LoadSettings(ctx)
	if err != nil {
		t.Fatalf("нечитаемое значение уронило загрузку настроек: %v", err)
	}
	if got.PollInterval != DefaultSettings().PollInterval {
		t.Errorf("не сработал откат к значению по умолчанию: %v", got.PollInterval)
	}
}

// Only one replica may poll; the rest must serve the UI and wait.
func TestPollerLockIsExclusive(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	acquired, release, err := s.TryLock(ctx, LockPoller)
	if err != nil || !acquired {
		t.Fatalf("первая блокировка не взята: %v", err)
	}
	again, release2, err := s.TryLock(ctx, LockPoller)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		release2()
		t.Fatal("вторая реплика тоже получила блокировку — опрос пошёл бы вдвое")
	}

	release()
	after, release3, err := s.TryLock(ctx, LockPoller)
	if err != nil || !after {
		t.Fatalf("после освобождения блокировка не берётся: %v", err)
	}
	release3()
}

func TestCountsReflectState(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "ok", "nospec", "down", "hidden")

	now := time.Now()
	s.RecordProbe(ctx, ProbeResult{ServiceID: ids["ok"], Status: ProbeOK, Succeeded: true, AutoProbe: true, CheckedAt: now})
	s.RecordProbe(ctx, ProbeResult{ServiceID: ids["nospec"], Status: ProbeNoSpec, AutoProbe: false, CheckedAt: now})
	s.RecordProbe(ctx, ProbeResult{ServiceID: ids["down"], Status: ProbeUnreachable, AutoProbe: true, CheckedAt: now})
	ignored := ReviewIgnored
	s.UpdateService(ctx, ids["hidden"], ServiceUpdate{ReviewState: &ignored})

	c, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Total != 4 || c.WithSpec != 1 || c.NoSpec != 1 || c.Unreachable != 1 || c.Hidden != 1 {
		t.Errorf("счётчики: %+v", c)
	}
	if c.ManualOnly != 1 {
		t.Errorf("сервис вне автоопроса не посчитан: %+v", c)
	}
}

func TestDiscoveryRunsRecordNamesAndFailures(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seed(t, s, "orders")

	if err := s.RecordSyncFailure(ctx, "kubernetes", context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListDiscoveryRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("записей о сканированиях %d, ожидалось 2", len(runs))
	}
	if runs[0].Error == "" {
		t.Error("неудачное сканирование записано без причины")
	}
	if len(runs[1].AddedNames) != 1 || runs[1].AddedNames[0] != "orders" {
		t.Errorf("имена появившихся сервисов не записаны: %+v", runs[1])
	}
}

// In the first months nothing declares an owner anywhere, so what a person
// types is the only source — and a cluster sweep must never wipe it.
func TestOwnerManualWinsPerField(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "orders")

	if err := s.UpdateService(ctx, ids["orders"], ServiceUpdate{
		Owner: &OwnerFields{Team: "Команда Профиля", Owners: []string{"Иванов"}},
	}); err != nil {
		t.Fatal(err)
	}
	// The specification later declares a different team and a repository.
	if err := s.SetDiscoveredOwner(ctx, ids["orders"], OwnerFields{
		Team: "Из спеки", Repo: "https://git/repo", Contact: "spec@company.ru",
	}); err != nil {
		t.Fatal(err)
	}

	svc, _ := s.GetService(ctx, ids["orders"])
	if svc.Owner.Team != "Команда Профиля" {
		t.Errorf("спека затёрла введённое руками: %q", svc.Owner.Team)
	}
	// Fields nobody filled in may be taken from the specification.
	if svc.Owner.Repo != "https://git/repo" || svc.Owner.Contact != "spec@company.ru" {
		t.Errorf("пустые поля не заполнились из спеки: %+v", svc.Owner)
	}
	if svc.Owner.Source != "manual" {
		t.Errorf("источник = %q, ожидался manual", svc.Owner.Source)
	}
	if svc.Owner.Manual.Team != "Команда Профиля" {
		t.Error("форма редактирования должна показывать введённое человеком")
	}
}

func TestOwnerFromSpecAloneIsMarkedAsSuch(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "orders")

	if err := s.SetDiscoveredOwner(ctx, ids["orders"], OwnerFields{Team: "Из спеки"}); err != nil {
		t.Fatal(err)
	}
	svc, _ := s.GetService(ctx, ids["orders"])
	if svc.Owner.Team != "Из спеки" || svc.Owner.Source != "spec" {
		t.Errorf("владелец из спеки: %+v", svc.Owner)
	}
	if !svc.Owner.Known() {
		t.Error("Known() не видит владельца")
	}
}

func TestCountsSeeMissingOwners(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ids := seed(t, s, "withowner", "without", "hidden")

	s.UpdateService(ctx, ids["withowner"], ServiceUpdate{Owner: &OwnerFields{Team: "Команда"}})
	ignored := ReviewIgnored
	s.UpdateService(ctx, ids["hidden"], ServiceUpdate{ReviewState: &ignored})

	c, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Hidden services are not something anyone needs to chase an owner for.
	if c.NoOwner != 1 {
		t.Errorf("без владельца: %d, ожидался 1 (спрятанные не считаются)", c.NoOwner)
	}
}

// ------------------------------------------------- слияние дублей и имена --

// Записи с общим публичным адресом остаются на месте — обе.
//
// Раньше портал выбирал «настоящую» и прятал остальные. Признак выбора включал
// число ревизий, а оно растёт, поэтому победитель переворачивался сам собой:
// сегодня по адресу видна одна запись, завтра другая. Человек, вписавший адрес
// спеки, назавтра смотрел уже на соседнюю запись и своего значения не находил.
func TestEntriesSharingAPublicAddressAreKept(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	ids := seed(t, s, "aaa", "bbb")

	both := []Discovered{
		{Key: "aaa.ns:80", DisplayName: "aaa", BaseURL: "http://aaa.ns:80",
			Host: "api.example.com", Namespace: "ns", Backend: "aaa.ns:80",
			PublicKey: "api.example.com"},
		{Key: "bbb.ns:80", DisplayName: "bbb", BaseURL: "http://bbb.ns:80",
			Host: "api.example.com", Namespace: "ns", Backend: "bbb.ns:80",
			PublicKey: "api.example.com"},
	}

	// Число ревизий у них меняется — раньше именно это переворачивало выбор.
	for round, extra := range []int64{0, ids["bbb"], ids["aaa"]} {
		if extra != 0 {
			if _, err := s.InsertVersion(ctx, &Version{ServiceID: extra,
				Hash: fmt.Sprintf("h%d", round), CapturedAt: time.Now(),
				Spec: []byte(`{"openapi":"3.0.0"}`)}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.SyncDiscovered(ctx, "kubernetes", both); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListServices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 {
			names := []string{}
			for _, v := range list {
				names = append(names, v.DisplayName)
			}
			t.Fatalf("проход %d: в списке %v, обе записи должны остаться", round, names)
		}
		for _, v := range list {
			if v.PublicKey != "api.example.com" {
				t.Errorf("проход %d: %s без публичного адреса — пометка о дубле не появится",
					round, v.DisplayName)
			}
		}
	}

	// И опрашиваются обе: скрытая запись переставала опрашиваться и «протухала».
	candidates, err := s.PollCandidates(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Errorf("опрашивается %d записей из двух", len(candidates))
	}
}

// Сценарий, из-за которого это и переделывается: адрес спеки, вписанный
// руками, обязан пережить сканирование кластера.
func TestManualSpecURLSurvivesASweepOnASharedAddress(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	ids := seed(t, s, "aaa", "bbb")

	manual := "http://aaa.ns:80/custom/openapi.json"
	if err := s.UpdateService(ctx, ids["aaa"], ServiceUpdate{ManualSpecURL: &manual}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncDiscovered(ctx, "kubernetes", []Discovered{
		{Key: "aaa.ns:80", DisplayName: "aaa", BaseURL: "http://aaa.ns:80",
			Host: "api.example.com", Namespace: "ns", Backend: "aaa.ns:80",
			PublicKey: "api.example.com"},
		{Key: "bbb.ns:80", DisplayName: "bbb", BaseURL: "http://bbb.ns:80",
			Host: "api.example.com", Namespace: "ns", Backend: "bbb.ns:80",
			PublicKey: "api.example.com"},
	}); err != nil {
		t.Fatal(err)
	}

	svc, err := s.GetService(ctx, ids["aaa"])
	if err != nil || svc == nil {
		t.Fatal(err)
	}
	if svc.ManualSpecURL != manual {
		t.Errorf("адрес спеки = %q, ожидался вписанный", svc.ManualSpecURL)
	}
	if !svc.Present {
		t.Error("запись пропала из списка после сканирования")
	}
}

// Название: имя backend-Service уступает заголовку из спеки, а вписанное
// человеком не уступает ничему.
func TestLabelPrefersTheSpecificationTitleOverTheServiceName(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	ids := seed(t, s, "api")

	if svc, _ := s.GetService(ctx, ids["api"]); svc.Label != "api" {
		t.Errorf("без спеки название = %q, ожидалось имя Service", svc.Label)
	}

	if _, err := s.InsertVersion(ctx, &Version{
		ServiceID: ids["api"], Hash: "h1", CapturedAt: time.Now(),
		Spec: []byte(`{"openapi":"3.0.0"}`), Title: "Заказы и доставка", EndpointCount: 3,
	}); err != nil {
		t.Fatal(err)
	}
	svc, _ := s.GetService(ctx, ids["api"])
	if svc.Label != "Заказы и доставка" {
		t.Errorf("название = %q, ожидался заголовок из спеки", svc.Label)
	}
	if svc.DisplayName != "api" {
		t.Errorf("исходное имя затёрлось: %q", svc.DisplayName)
	}

	name := "Ядро заказов"
	if err := s.UpdateService(ctx, ids["api"], ServiceUpdate{DisplayName: &name}); err != nil {
		t.Fatal(err)
	}
	if svc, _ := s.GetService(ctx, ids["api"]); svc.Label != name {
		t.Errorf("вписанное имя уступило заголовку: %q", svc.Label)
	}

	// Очистка поля возвращает заголовок из спеки, а не пустое имя.
	empty := ""
	if err := s.UpdateService(ctx, ids["api"], ServiceUpdate{DisplayName: &empty}); err != nil {
		t.Fatal(err)
	}
	svc, _ = s.GetService(ctx, ids["api"])
	if svc.Label != "Заказы и доставка" {
		t.Errorf("после очистки название = %q", svc.Label)
	}
	if svc.DisplayName != "api" {
		t.Errorf("после очистки имя = %q, ожидалось имя backend-Service", svc.DisplayName)
	}
}

// Восстановление того, что успела спрятать предыдущая версия.
//
// На проде записи уже помечены поглощёнными и убраны из списков. Миграция 010
// обязана вернуть ровно их и не тронуть те, что отсутствуют в кластере
// по-настоящему.
func TestMigrationRestoresWhatTheMergeHid(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	ids := seed(t, s, "winner", "hidden", "really-gone")

	// Приводим базу к состоянию, которое оставила прошлая версия.
	if _, err := s.Pool().Exec(ctx, `
		UPDATE services SET superseded_by = $1, present = FALSE, auto_probe = FALSE
		WHERE id = $2`, ids["winner"], ids["hidden"]); err != nil {
		t.Fatal(err)
	}
	// А эта запись пропала из кластера сама — её трогать нельзя.
	if _, err := s.Pool().Exec(ctx,
		`UPDATE services SET present = FALSE WHERE id = $1`, ids["really-gone"]); err != nil {
		t.Fatal(err)
	}
	// Откатываем отметку о миграции, чтобы она применилась к этому состоянию.
	if _, err := s.Pool().Exec(ctx, `DELETE FROM schema_migrations WHERE version = 10`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("миграция не применилась: %v", err)
	}

	restored, err := s.GetService(ctx, ids["hidden"])
	if err != nil || restored == nil {
		t.Fatal(err)
	}
	if !restored.Present || !restored.AutoProbe {
		t.Errorf("спрятанная запись не восстановлена: present=%v autoProbe=%v",
			restored.Present, restored.AutoProbe)
	}

	gone, err := s.GetService(ctx, ids["really-gone"])
	if err != nil || gone == nil {
		t.Fatal(err)
	}
	if gone.Present {
		t.Error("запись, отсутствующая в кластере, помечена присутствующей")
	}

	// И она снова в списке — ради этого всё и делалось.
	list, err := s.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range list {
		if v.ID == ids["hidden"] {
			found = true
		}
	}
	if !found {
		t.Error("восстановленной записи нет в списке")
	}
}
