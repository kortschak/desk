module github.com/kortschak/desk

go 1.22

require (
	github.com/soypat/cyw43439 v0.0.0-20250106095300-90bf0c1db251
	github.com/soypat/seqs v0.0.0-20240527012110-1201bab640ef
	tinygo.org/x/bluetooth v0.0.0-00010101000000-000000000000
)

require (
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/saltosystems/winrt-go v0.0.0-20240509164145-4f7860a3bd2b // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/tinygo-org/cbgo v0.0.4 // indirect
	github.com/tinygo-org/pio v0.0.0-20231216154340-cd888eb58899 // indirect
	golang.org/x/exp v0.0.0-20230728194245-b0cb94b80691 // indirect
	golang.org/x/sys v0.11.0 // indirect
)

// Necessary to work around API/device impedance mismatch.
replace tinygo.org/x/bluetooth => github.com/kortschak/bluetooth v0.0.0-20250119004136-a89c13d81bcc
