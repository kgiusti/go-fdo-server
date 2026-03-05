// SPDX-FileCopyrightText: (C) 2024 Intel Corporation
// SPDX-License-Identifier: Apache 2.0

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/fido-device-onboard/go-fdo"
	"github.com/fido-device-onboard/go-fdo/cbor"
	"github.com/fido-device-onboard/go-fdo/protocol"
	"gorm.io/gorm"
)

// OwnerVoucherPersistentState implementation

// AddVoucher stores the voucher of a device owned by the service
func (s *State) AddVoucher(ctx context.Context, ov *fdo.Voucher) error {
	voucherBytes, err := cbor.Marshal(ov)
	if err != nil {
		return fmt.Errorf("failed to marshal voucher: %w", err)
	}

	now := time.Now()
	voucher := Voucher{
		GUID:       ov.Header.Val.GUID[:],
		DeviceInfo: ov.Header.Val.DeviceInfo,
		CBOR:       voucherBytes,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	return s.DB.Create(&voucher).Error
}

// ReplaceVoucher stores a new voucher, possibly deleting or marking the previous voucher as replaced
func (s *State) ReplaceVoucher(ctx context.Context, guid protocol.GUID, ov *fdo.Voucher) error {
	voucherBytes, err := cbor.Marshal(ov)
	if err != nil {
		return fmt.Errorf("failed to marshal voucher: %w", err)
	}

	now := time.Now()
	voucher := Voucher{
		GUID:       ov.Header.Val.GUID[:],
		DeviceInfo: ov.Header.Val.DeviceInfo,
		CBOR:       voucherBytes,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// Mark TO2 completion for this GUID and record new GUID that changed
	completedAt := time.Now()
	replacement := DeviceOnboarding{GUID: guid[:], NewGUID: ov.Header.Val.GUID[:], TO2Completed: true, TO2CompletedAt: &completedAt}

	return s.DB.Transaction(func(tx *gorm.DB) error {
		// Delete the old voucher row (by original GUID), then create the new voucher
		if err := tx.Where("guid = ?", guid[:]).Delete(&Voucher{}).Error; err != nil {
			return err
		}
		if err := tx.Create(&voucher).Error; err != nil {
			return err
		}
		// Update onboarding completion and new GUID
		return tx.Where("guid = ?", guid[:]).
			Assign(replacement).
			FirstOrCreate(&DeviceOnboarding{}).Error
	})
}

// RemoveVoucher untracks a voucher, possibly by deleting it or marking it as removed
// TODO: we should mark the voucher as removed instead of deleting it
func (s *State) RemoveVoucher(ctx context.Context, guid protocol.GUID) (*fdo.Voucher, error) {
	var ov fdo.Voucher
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		var voucher Voucher
		if err := tx.Where("guid = ?", guid[:]).First(&voucher).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return fdo.ErrNotFound
			}
			return err
		}
		// Parse the voucher before deleting
		if err := cbor.Unmarshal(voucher.CBOR, &ov); err != nil {
			return fmt.Errorf("failed to unmarshal voucher: %w", err)
		}
		// Delete the voucher
		if err := tx.Where("guid = ?", guid[:]).Delete(&Voucher{}).Error; err != nil {
			return err
		}
		// Delete the onboarding tracking row for this GUID (best-effort)
		return tx.Where("guid = ?", guid[:]).Delete(&DeviceOnboarding{}).Error
	}); err != nil {
		return nil, err
	}
	return &ov, nil
}

// Voucher retrieves a voucher by GUID
func (s *State) Voucher(ctx context.Context, guid protocol.GUID) (*fdo.Voucher, error) {
	var voucher Voucher
	if err := s.DB.Where("guid = ?", guid[:]).First(&voucher).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, fdo.ErrNotFound
		}
		return nil, err
	}

	var ov fdo.Voucher
	if err := cbor.Unmarshal(voucher.CBOR, &ov); err != nil {
		return nil, fmt.Errorf("failed to unmarshal voucher: %w", err)
	}

	return &ov, nil
}
